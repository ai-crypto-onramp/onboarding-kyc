package internal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Application is the aggregate root for a KYC application.
type Application struct {
	ID                string    `json:"id"`
	UserID            string    `json:"user_id"`
	Vendor            string    `json:"vendor,omitempty"`
	VendorApplicantID string    `json:"vendor_applicant_id,omitempty"`
	State             State     `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	ReKYCDueAt        time.Time `json:"re_kyc_due_at,omitempty"`
	DecidedAt         time.Time `json:"decided_at,omitempty"`
	Version           int       `json:"version"`
}

// Document is metadata for an uploaded document.
type Document struct {
	ID            string    `json:"id"`
	ApplicationID string    `json:"application_id"`
	Type          string    `json:"type"`
	Content       []byte    `json:"-"`
	UploadedAt    time.Time `json:"uploaded_at"`
	RetentionUntil time.Time `json:"retention_until"`
}

// LivenessSession is a vendor liveness session for an application.
type LivenessSession struct {
	ID              string    `json:"id"`
	ApplicationID   string    `json:"application_id"`
	VendorSessionID string    `json:"vendor_session_id"`
	Status          string    `json:"status"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	Result          string    `json:"result,omitempty"`
	RetentionUntil  time.Time `json:"retention_until,omitempty"`
}

// SanctionsHit is a screening match persisted for analyst review.
type SanctionsHit struct {
	ID            string    `json:"id"`
	ApplicationID string    `json:"application_id"`
	List          string    `json:"list"`
	MatchedName   string    `json:"matched_name"`
	Score         float64   `json:"score"`
	ReviewedBy    string    `json:"reviewed_by,omitempty"`
	ReviewedAt    time.Time `json:"reviewed_at,omitempty"`
	Disposition   string    `json:"disposition,omitempty"`
}

// Decision is the final KYC decision for an application.
type Decision struct {
	ID            string    `json:"id"`
	ApplicationID string    `json:"application_id"`
	Outcome       string    `json:"outcome"`
	Reason        string    `json:"reason,omitempty"`
	DecidedBy     string    `json:"decided_by"`
	DecidedAt     time.Time `json:"decided_at"`
}

// ErrNotFound is returned when an entity is not found.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on optimistic concurrency conflict.
var ErrConflict = errors.New("conflict")

// ErrDuplicate is returned when a uniqueness constraint is violated.
var ErrDuplicate = errors.New("duplicate")

// ErrInvalidArgument is returned for invalid input.
var ErrInvalidArgument = errors.New("invalid argument")

// EventSink is the interface implemented by the audit log for consuming
// in-memory domain events.
type EventSink interface {
	RecordTransition(appID string, from, to State, actor, reason string)
}

// noopEventSink ignores all events.
type noopEventSink struct{}

func (noopEventSink) RecordTransition(string, State, State, string, string) {}

// ApplicationRepository is the in-memory application store.
type ApplicationRepository struct {
	mu             sync.Mutex
	apps           map[string]*Application
	byUser         map[string]string
	sink           EventSink
	now            func() time.Time
}

// NewApplicationRepository creates a new in-memory repository.
func NewApplicationRepository(sink EventSink) *ApplicationRepository {
	if sink == nil {
		sink = noopEventSink{}
	}
	return &ApplicationRepository{
		apps:   make(map[string]*Application),
		byUser: make(map[string]string),
		sink:   sink,
		now:    time.Now,
	}
}

// Create inserts a new application. Returns ErrDuplicate if user_id already
// has an application that is not terminal (only one active per user).
func (r *ApplicationRepository) Create(app *Application) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if app.UserID == "" {
		return fmt.Errorf("%w: user_id is required", ErrInvalidArgument)
	}
	if existingID, ok := r.byUser[app.UserID]; ok {
		ex := r.apps[existingID]
		if ex != nil && !IsTerminal(ex.State) {
			return fmt.Errorf("%w: active application already exists for user %s", ErrDuplicate, app.UserID)
		}
		// replace terminal app: remove old mapping
		delete(r.apps, existingID)
	}
	if app.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidArgument)
	}
	if _, exists := r.apps[app.ID]; exists {
		return fmt.Errorf("%w: application id %s", ErrDuplicate, app.ID)
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
	r.apps[app.ID] = app
	r.byUser[app.UserID] = app.ID
	return nil
}

// Get returns a copy of the application by id.
func (r *ApplicationRepository) Get(id string) (*Application, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	app, ok := r.apps[id]
	if !ok {
		return nil, fmt.Errorf("%w: application %s", ErrNotFound, id)
	}
	cp := *app
	return &cp, nil
}

// GetByUserID returns a copy of the application by user id.
func (r *ApplicationRepository) GetByUserID(userID string) (*Application, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byUser[userID]
	if !ok {
		return nil, fmt.Errorf("%w: application for user %s", ErrNotFound, userID)
	}
	app, ok := r.apps[id]
	if !ok {
		return nil, fmt.Errorf("%w: application for user %s", ErrNotFound, userID)
	}
	cp := *app
	return &cp, nil
}

// UpdateState transitions the application to newState, guarded by the
// transition table and optimistic concurrency via the version field.
func (r *ApplicationRepository) UpdateState(id string, version int, newState State, actor, reason string) (*Application, error) {
	_, span := startSpan(context.Background(), "repo.UpdateState")
	defer span.End()
	r.mu.Lock()
	defer r.mu.Unlock()
	app, ok := r.apps[id]
	if !ok {
		return nil, fmt.Errorf("%w: application %s", ErrNotFound, id)
	}
	if app.Version != version {
		return nil, fmt.Errorf("%w: application %s version mismatch (have %d want %d)", ErrConflict, id, app.Version, version)
	}
	from := app.State
	if err := ValidateTransition(from, newState); err != nil {
		return nil, err
	}
	app.State = newState
	app.Version++
	app.UpdatedAt = r.now()
	if IsTerminal(newState) {
		app.DecidedAt = app.UpdatedAt
		app.ReKYCDueAt = app.DecidedAt.Add(365 * 24 * time.Hour)
	}
	r.sink.RecordTransition(id, from, newState, actor, reason)
	cp := *app
	return &cp, nil
}

// Reopen moves a terminal application back to started for re-KYC.
func (r *ApplicationRepository) Reopen(id string, version int, actor string) (*Application, error) {
	_, span := startSpan(context.Background(), "repo.Reopen")
	defer span.End()
	r.mu.Lock()
	defer r.mu.Unlock()
	app, ok := r.apps[id]
	if !ok {
		return nil, fmt.Errorf("%w: application %s", ErrNotFound, id)
	}
	if app.Version != version {
		return nil, fmt.Errorf("%w: application %s version mismatch", ErrConflict, id)
	}
	if !IsTerminal(app.State) {
		return nil, ErrReKYCNotTerminal
	}
	from := app.State
	app.State = StateStarted
	app.Version++
	app.UpdatedAt = r.now()
	app.DecidedAt = time.Time{}
	app.ReKYCDueAt = time.Time{}
	r.sink.RecordTransition(id, from, StateStarted, actor, "re-kyc")
	cp := *app
	return &cp, nil
}

// SetVendorApplicantID stores the vendor applicant id and bumps the version.
func (r *ApplicationRepository) SetVendorApplicantID(id, vendorID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if app, ok := r.apps[id]; ok {
		app.VendorApplicantID = vendorID
		app.Version++
		app.UpdatedAt = r.now()
	}
}

// ListDueForReKYC returns ids+versions of applications whose re_kyc_due_at
// is set and in the past.
func (r *ApplicationRepository) ListDueForReKYC(now time.Time) []ReKYCDue {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ReKYCDue
	for _, app := range r.apps {
		if !app.ReKYCDueAt.IsZero() && !app.ReKYCDueAt.After(now) && IsTerminal(app.State) {
			out = append(out, ReKYCDue{ID: app.ID, Version: app.Version})
		}
	}
	return out
}