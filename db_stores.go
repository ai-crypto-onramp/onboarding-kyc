package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ai-crypto-onramp/onboarding-kyc/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// openPoolAndMigrate opens a pgxpool against dsn and applies all pending
// migrations.
func openPoolAndMigrate(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg := db.DefaultConfig()
	cfg.DSN = dsn
	pool, err := db.Pool(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := db.MigrateUp(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return pool, nil
}

// errNoRows wraps pgx.ErrNoRows into a sentinel compatible with the
// in-memory error mapping.
func errNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// --- ApplicationRepo ---

// DBApplicationRepo implements ApplicationRepo against a pgxpool.Pool.
type DBApplicationRepo struct {
	pool *pgxpool.Pool
	sink EventSink
	now  func() time.Time
}

// NewDBApplicationRepo creates a new DB-backed application repository.
func NewDBApplicationRepo(pool *pgxpool.Pool, sink EventSink) *DBApplicationRepo {
	if sink == nil {
		sink = noopEventSink{}
	}
	return &DBApplicationRepo{pool: pool, sink: sink, now: time.Now}
}

// Create inserts a new application. Returns ErrDuplicate if the user already
// has a non-terminal application.
func (r *DBApplicationRepo) Create(app *Application) error {
	if app.UserID == "" {
		return fmt.Errorf("%w: user_id is required", ErrInvalidArgument)
	}
	if app.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidArgument)
	}
	if app.State == "" {
		app.State = StateStarted
	}
	now := r.now()
	if app.CreatedAt.IsZero() {
		app.CreatedAt = now
	}
	app.UpdatedAt = now
	if app.Version == 0 {
		app.Version = 1
	}
	ctx := context.Background()
	// Enforce one active application per user: if the user has a non-terminal
	// app, reject; if they have a terminal app, delete it first.
	var existingID string
	var existingState State
	err := r.pool.QueryRow(ctx,
		"SELECT id, state FROM kyc_applications WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1",
		app.UserID).Scan(&existingID, &existingState)
	if err == nil {
		if !IsTerminal(existingState) {
			return fmt.Errorf("%w: active application already exists for user %s", ErrDuplicate, app.UserID)
		}
		// remove terminal app to make room for the new one
		if _, derr := r.pool.Exec(ctx, "DELETE FROM kyc_applications WHERE id=$1", existingID); derr != nil {
			return fmt.Errorf("delete terminal app: %w", derr)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = r.pool.Exec(ctx, `
INSERT INTO kyc_applications (id, user_id, vendor, vendor_application_id, state, created_at, updated_at, expires_at, re_kyc_due_at, decided_at, version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		app.ID, app.UserID, nullableString(app.Vendor), nullableString(app.VendorApplicantID),
		string(app.State), app.CreatedAt, app.UpdatedAt, nullableTime(app.ExpiresAt),
		nullableTime(app.ReKYCDueAt), nullableTime(app.DecidedAt), app.Version)
	if err != nil {
		// unique violation on the partial index for active user
		return fmt.Errorf("%w: %v", ErrDuplicate, err)
	}
	return nil
}

// Get returns a copy of the application by id.
func (r *DBApplicationRepo) Get(id string) (*Application, error) {
	ctx := context.Background()
	row := r.pool.QueryRow(ctx, `
SELECT id, user_id, vendor, vendor_application_id, state, created_at, updated_at, expires_at, re_kyc_due_at, decided_at, version
FROM kyc_applications WHERE id=$1`, id)
	return scanApp(row)
}

// GetByUserID returns the application by user id.
func (r *DBApplicationRepo) GetByUserID(userID string) (*Application, error) {
	ctx := context.Background()
	row := r.pool.QueryRow(ctx, `
SELECT id, user_id, vendor, vendor_application_id, state, created_at, updated_at, expires_at, re_kyc_due_at, decided_at, version
FROM kyc_applications WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1`, userID)
	return scanApp(row)
}

// UpdateState transitions the application to newState with optimistic
// concurrency on the version field.
func (r *DBApplicationRepo) UpdateState(id string, version int, newState State, actor, reason string) (*Application, error) {
	ctx := context.Background()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var app Application
	row := tx.QueryRow(ctx, `
SELECT id, user_id, vendor, vendor_application_id, state, created_at, updated_at, expires_at, re_kyc_due_at, decided_at, version
FROM kyc_applications WHERE id=$1 FOR UPDATE`, id)
	app, err = scanRow(row)
	if err != nil {
		return nil, errNoRows(err)
	}
	if app.Version != version {
		return nil, fmt.Errorf("%w: application %s version mismatch (have %d want %d)", ErrConflict, id, app.Version, version)
	}
	from := app.State
	if err := ValidateTransition(from, newState); err != nil {
		return nil, err
	}
	now := r.now()
	app.State = newState
	app.Version++
	app.UpdatedAt = now
	var decidedAt, reKYCDueAt *time.Time
	if IsTerminal(newState) {
		app.DecidedAt = now
		app.ReKYCDueAt = now.Add(365 * 24 * time.Hour)
		decidedAt = &now
		rd := app.ReKYCDueAt
		reKYCDueAt = &rd
	}
	if _, err := tx.Exec(ctx, `
UPDATE kyc_applications
SET state=$2, updated_at=$3, version=$4, decided_at=COALESCE($5, decided_at), re_kyc_due_at=COALESCE($6, re_kyc_due_at)
WHERE id=$1 AND version=$7`,
		id, string(newState), now, app.Version, decidedAt, reKYCDueAt, version); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	r.sink.RecordTransition(id, from, newState, actor, reason)
	return &app, nil
}

// Reopen moves a terminal application back to started for re-KYC.
func (r *DBApplicationRepo) Reopen(id string, version int, actor string) (*Application, error) {
	ctx := context.Background()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var app Application
	row := tx.QueryRow(ctx, `
SELECT id, user_id, vendor, vendor_application_id, state, created_at, updated_at, expires_at, re_kyc_due_at, decided_at, version
FROM kyc_applications WHERE id=$1 FOR UPDATE`, id)
	app, err = scanRow(row)
	if err != nil {
		return nil, errNoRows(err)
	}
	if app.Version != version {
		return nil, fmt.Errorf("%w: application %s version mismatch", ErrConflict, id)
	}
	if !IsTerminal(app.State) {
		return nil, ErrReKYCNotTerminal
	}
	from := app.State
	now := r.now()
	app.State = StateStarted
	app.Version++
	app.UpdatedAt = now
	app.DecidedAt = time.Time{}
	app.ReKYCDueAt = time.Time{}
	if _, err := tx.Exec(ctx, `
UPDATE kyc_applications
SET state=$2, updated_at=$3, version=$4, decided_at=NULL, re_kyc_due_at=NULL
WHERE id=$1 AND version=$5`,
		id, string(StateStarted), now, app.Version, version); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	r.sink.RecordTransition(id, from, StateStarted, actor, "re-kyc")
	return &app, nil
}

// ListDueForReKYC returns applications whose re_kyc_due_at is set and in the
// past.
func (r *DBApplicationRepo) ListDueForReKYC(now time.Time) []ReKYCDue {
	ctx := context.Background()
	rows, err := r.pool.Query(ctx, `
SELECT id, version FROM kyc_applications
WHERE re_kyc_due_at IS NOT NULL AND re_kyc_due_at <= $1 AND state IN ('pass','fail')`, now)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ReKYCDue
	for rows.Next() {
		var d ReKYCDue
		if err := rows.Scan(&d.ID, &d.Version); err != nil {
			return out
		}
		out = append(out, d)
	}
	return out
}

// SetVendorApplicantID stores the vendor applicant id and bumps the version.
func (r *DBApplicationRepo) SetVendorApplicantID(id, vendorID string) {
	ctx := context.Background()
	_, _ = r.pool.Exec(ctx, `
UPDATE kyc_applications SET vendor_application_id=$2, version=version+1, updated_at=$3 WHERE id=$1`,
		id, vendorID, r.now())
}

// --- DocumentRepo ---

// DBDocumentRepo implements DocumentRepo against a pgxpool.Pool. Document
// content is stored base64-encoded in the object_key column (the schema does
// not have a dedicated content column).
type DBDocumentRepo struct {
	pool *pgxpool.Pool
}

// NewDBDocumentRepo creates a new DB-backed document repository.
func NewDBDocumentRepo(pool *pgxpool.Pool) *DBDocumentRepo {
	return &DBDocumentRepo{pool: pool}
}

// Add stores a document. The content is base64-encoded into object_key.
func (d *DBDocumentRepo) Add(appID string, doc Document) {
	ctx := context.Background()
	objKey := base64.StdEncoding.EncodeToString(doc.Content)
	_, _ = d.pool.Exec(ctx, `
INSERT INTO documents (id, application_id, type, object_key, uploaded_at, retention_until)
VALUES ($1,$2,$3,$4,$5,$6)`,
		doc.ID, appID, doc.Type, objKey, doc.UploadedAt, doc.RetentionUntil)
}

// List returns documents for an application, decoding content from object_key.
func (d *DBDocumentRepo) List(appID string) []Document {
	ctx := context.Background()
	rows, err := d.pool.Query(ctx, `
SELECT id, application_id, type, object_key, uploaded_at, retention_until
FROM documents WHERE application_id=$1 ORDER BY uploaded_at`, appID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		var doc Document
		var objKey *string
		if err := rows.Scan(&doc.ID, &doc.ApplicationID, &doc.Type, &objKey, &doc.UploadedAt, &doc.RetentionUntil); err != nil {
			return out
		}
		if objKey != nil {
			if b, err := base64.StdEncoding.DecodeString(*objKey); err == nil {
				doc.Content = b
			}
		}
		out = append(out, doc)
	}
	return out
}

// HasRequiredDocs returns true if at least id_front + selfie are present.
func (d *DBDocumentRepo) HasRequiredDocs(appID string) bool {
	ctx := context.Background()
	for _, t := range []string{"id_front", "selfie"} {
		var exists bool
		err := d.pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM documents WHERE application_id=$1 AND type=$2)", appID, t).Scan(&exists)
		if err != nil || !exists {
			return false
		}
	}
	return true
}

// SweepExpired removes and returns the count of documents whose
// retention_until is in the past.
func (d *DBDocumentRepo) SweepExpired(now time.Time) int {
	ctx := context.Background()
	ctag, err := d.pool.Exec(ctx,
		"DELETE FROM documents WHERE retention_until IS NOT NULL AND retention_until <= $1", now)
	if err != nil {
		return 0
	}
	return int(ctag.RowsAffected())
}

// --- LivenessRepo ---

// DBLivenessRepo implements LivenessRepo against a pgxpool.Pool.
type DBLivenessRepo struct {
	pool *pgxpool.Pool
}

// NewDBLivenessRepo creates a new DB-backed liveness repository.
func NewDBLivenessRepo(pool *pgxpool.Pool) *DBLivenessRepo {
	return &DBLivenessRepo{pool: pool}
}

// Add stores a liveness session. The result column is jsonb in Postgres, so
// the plain-string Result field is JSON-encoded before being stored. Any
// insert error is logged; the LivenessRepo interface does not return an
// error, but silent failures here caused GET /liveness to 404 immediately
// after a successful POST.
func (l *DBLivenessRepo) Add(appID string, s LivenessSession) {
	ctx := context.Background()
	resultJSON, err := json.Marshal(s.Result)
	if err != nil {
		slog.Error("liveness add: encode result", "app_id", appID, "err", err)
		return
	}
	if _, err := l.pool.Exec(ctx, `
INSERT INTO liveness_sessions (id, application_id, vendor_session_id, status, started_at, completed_at, result, retention_until)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		s.ID, appID, nullableString(s.VendorSessionID), s.Status, s.StartedAt,
		nullableTime(s.CompletedAt), resultJSON, nullableTime(s.RetentionUntil)); err != nil {
		slog.Error("liveness add: insert failed", "app_id", appID, "err", err)
	}
}

// Latest returns the most recent liveness session for an application. The
// result column is jsonb; a scalar JSON string (e.g. "pass") is decoded back
// into the plain-string Result field.
func (l *DBLivenessRepo) Latest(appID string) (LivenessSession, bool) {
	ctx := context.Background()
	row := l.pool.QueryRow(ctx, `
SELECT id, application_id, vendor_session_id, status, started_at, completed_at, result, retention_until
FROM liveness_sessions WHERE application_id=$1 ORDER BY started_at DESC LIMIT 1`, appID)
	var s LivenessSession
	var vendorSessionID *string
	var result []byte
	var completedAt, retentionUntil *time.Time
	err := row.Scan(&s.ID, &s.ApplicationID, &vendorSessionID, &s.Status, &s.StartedAt,
		&completedAt, &result, &retentionUntil)
	if err != nil {
		return LivenessSession{}, false
	}
	if vendorSessionID != nil {
		s.VendorSessionID = *vendorSessionID
	}
	if len(result) > 0 {
		// The result column is jsonb. A scalar string is stored as a JSON
		// string literal ("pass"), so unmarshal into a string. If it is a
		// JSON object/array, fall back to the raw text.
		if jerr := json.Unmarshal(result, &s.Result); jerr != nil {
			s.Result = string(result)
		}
	}
	if completedAt != nil {
		s.CompletedAt = *completedAt
	}
	if retentionUntil != nil {
		s.RetentionUntil = *retentionUntil
	}
	return s, true
}

// SweepExpired removes and returns the count of liveness sessions whose
// retention_until is set and in the past.
func (l *DBLivenessRepo) SweepExpired(now time.Time) int {
	ctx := context.Background()
	ctag, err := l.pool.Exec(ctx,
		"DELETE FROM liveness_sessions WHERE retention_until IS NOT NULL AND retention_until <= $1", now)
	if err != nil {
		return 0
	}
	return int(ctag.RowsAffected())
}

// --- helpers ---

// scanApp scans a full application row, mapping NULLs to zero values.
func scanApp(row pgx.Row) (*Application, error) {
	a, err := scanRow(row)
	if err != nil {
		return nil, errNoRows(err)
	}
	return &a, nil
}

// scanRow scans a full application row into an Application value.
func scanRow(row pgx.Row) (Application, error) {
	var a Application
	var vendor, vendorApplicantID *string
	var expiresAt, reKYCDueAt, decidedAt *time.Time
	err := row.Scan(
		&a.ID, &a.UserID, &vendor, &vendorApplicantID, &a.State,
		&a.CreatedAt, &a.UpdatedAt, &expiresAt, &reKYCDueAt, &decidedAt, &a.Version)
	if err != nil {
		return a, err
	}
	if vendor != nil {
		a.Vendor = *vendor
	}
	if vendorApplicantID != nil {
		a.VendorApplicantID = *vendorApplicantID
	}
	if expiresAt != nil {
		a.ExpiresAt = *expiresAt
	}
	if reKYCDueAt != nil {
		a.ReKYCDueAt = *reKYCDueAt
	}
	if decidedAt != nil {
		a.DecidedAt = *decidedAt
	}
	return a, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}