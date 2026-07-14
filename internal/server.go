package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AppError is a typed API error that carries a code and status code.
type AppError struct {
	Code       string
	Message    string
	StatusCode int
}

func (e *AppError) Error() string { return e.Message }

// NewAppError constructs an AppError.
func NewAppError(code, msg string, status int) *AppError {
	return &AppError{Code: code, Message: msg, StatusCode: status}
}

var (
	errAppNotFound     = NewAppError("application_not_found", "application not found", http.StatusNotFound)
	errUserNotFound    = NewAppError("user_not_found", "user not found", http.StatusNotFound)
	errBadJSON         = NewAppError("bad_json", "invalid JSON body", http.StatusBadRequest)
	errMissingUserID   = NewAppError("missing_user_id", "user_id is required", http.StatusBadRequest)
	errBadDocType      = NewAppError("bad_doc_type", "invalid document type", http.StatusBadRequest)
	errDupApp          = NewAppError("duplicate_application", "active application already exists for user", http.StatusConflict)
	errConflict        = NewAppError("version_conflict", "application was modified by another request", http.StatusConflict)
	errIllegalTrans    = NewAppError("illegal_transition", "illegal state transition", http.StatusConflict)
	errInternal        = NewAppError("internal_error", "internal server error", http.StatusInternalServerError)
)

// errorEnvelope is the standard error response body.
type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// writeError writes an AppError as JSON with the right status.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	ae := toAppError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.StatusCode)
	rid := requestIDFromContext(r.Context())
	var env errorEnvelope
	env.Error.Code = ae.Code
	env.Error.Message = ae.Message
	env.Error.RequestID = rid
	_ = json.NewEncoder(w).Encode(env)
}

// toAppError maps errors to AppError.
func toAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	var ae *AppError
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return errAppNotFound
	case errors.Is(err, ErrDuplicate):
		return errDupApp
	case errors.Is(err, ErrConflict):
		return errConflict
	case errors.Is(err, ErrIllegalTransition):
		return errIllegalTrans
	case errors.Is(err, ErrReKYCNotTerminal):
		return NewAppError("not_terminal", err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInvalidArgument):
		return NewAppError("invalid_argument", err.Error(), http.StatusBadRequest)
	case errors.Is(err, errBadDisposition):
		return errBadDisposition
	case errors.Is(err, errNotInReview):
		return errNotInReview
	}
	return errInternal
}

// ctxKey is an unexported context key type.
type ctxKey int

const (
	keyRequestID ctxKey = iota
)

// requestIDFromContext returns the request id stored in the context, or "".
func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

// correlationMiddleware sets X-Correlation-Id (uses existing or generates a
// new one) and stores it in context as the request_id.
func correlationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Correlation-Id")
		if rid == "" {
			rid = newUUID()
		}
		w.Header().Set("X-Correlation-Id", rid)
		ctx := context.WithValue(r.Context(), keyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loggingMiddleware logs each request as structured JSON and records metrics.
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := requestIDFromContext(r.Context())
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		globalMetrics.requestsTotal.Add(1)
		globalMetrics.observeLatency(time.Since(start))
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", rid,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Services bundles all dependencies used by the HTTP handlers.
type Services struct {
	Repo     ApplicationRepo
	Docs     DocumentRepo
	Liveness LivenessRepo
	Screen   *ScreeningService
	Webhook  *WebhookService
	Audit    *AuditLog
	Vendor   VendorClient
	ReKYC    *ReKYCService
}

// DocumentStore holds uploaded documents and liveness sessions in memory.
type DocumentStore struct {
	mu  sync.Mutex
	docs map[string][]Document
}

// NewDocumentStore creates a new DocumentStore.
func NewDocumentStore() *DocumentStore {
	return &DocumentStore{docs: make(map[string][]Document)}
}

// Add stores a document.
func (d *DocumentStore) Add(appID string, doc Document) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs[appID] = append(d.docs[appID], doc)
}

// List returns documents for an application.
func (d *DocumentStore) List(appID string) []Document {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Document, len(d.docs[appID]))
	copy(out, d.docs[appID])
	return out
}

// HasRequiredDocs returns true if at least id_front + selfie are present.
func (d *DocumentStore) HasRequiredDocs(appID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	var hasID, hasSelfie bool
	for _, doc := range d.docs[appID] {
		switch doc.Type {
		case "id_front":
			hasID = true
		case "selfie":
			hasSelfie = true
		}
	}
	return hasID && hasSelfie
}

// SweepExpired removes and returns the count of documents whose
// retention_until is in the past relative to now. Removed documents are
// hard-deleted (content zeroed then dropped from the map).
func (d *DocumentStore) SweepExpired(now time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	removed := 0
	for appID, docs := range d.docs {
		kept := docs[:0]
		for _, doc := range docs {
			if !doc.RetentionUntil.IsZero() && !doc.RetentionUntil.After(now) {
				// redact content before dropping
				for i := range doc.Content {
					doc.Content[i] = 0
				}
				removed++
				continue
			}
			kept = append(kept, doc)
		}
		d.docs[appID] = kept
	}
	return removed
}

// LivenessStore holds liveness sessions in memory.
type LivenessStore struct {
	mu       sync.Mutex
	sessions map[string][]LivenessSession
}

// NewLivenessStore creates a new LivenessStore.
func NewLivenessStore() *LivenessStore {
	return &LivenessStore{sessions: make(map[string][]LivenessSession)}
}

// Add stores a liveness session.
func (l *LivenessStore) Add(appID string, s LivenessSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions[appID] = append(l.sessions[appID], s)
}

// Latest returns the most recent liveness session for an application.
func (l *LivenessStore) Latest(appID string) (LivenessSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessions[appID]
	if len(s) == 0 {
		return LivenessSession{}, false
	}
	return s[len(s)-1], true
}

// SweepExpired removes and returns the count of liveness sessions whose
// retention_until is set and in the past relative to now.
func (l *LivenessStore) SweepExpired(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for appID, sessions := range l.sessions {
		kept := sessions[:0]
		for _, s := range sessions {
			if !s.RetentionUntil.IsZero() && !s.RetentionUntil.After(now) {
				removed++
				continue
			}
			kept = append(kept, s)
		}
		l.sessions[appID] = kept
	}
	return removed
}

// newServices wires services. If DB_URL is set it opens a pgxpool, runs
// migrations, and uses DB-backed stores; otherwise it falls back to the
// in-memory stores.
func newServices() *Services {
	audit := NewAuditLog()
	// EventSink for state transitions: a PolicyEventSink if
	// POLICY_RISK_ENGINE_URL is set, otherwise the audit log itself (which
	// already records transitions). The PolicyEventSink also fans out to the
	// audit log via a composite sink.
	var sink EventSink = audit
	if os.Getenv("POLICY_RISK_ENGINE_URL") != "" {
		logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		policy := NewPolicyEventSink(logger)
		sink = &compositeEventSink{primary: policy, secondary: audit}
	}

	var (
		repo     ApplicationRepo
		docs     DocumentRepo
		liveness LivenessRepo
	)
	if dsn := os.Getenv("DB_URL"); dsn != "" {
		pool, err := openPoolAndMigrate(context.Background(), dsn)
		if err != nil {
			panic(err)
		}
		repo = NewDBApplicationRepo(pool, sink)
		docs = NewDBDocumentRepo(pool)
		liveness = NewDBLivenessRepo(pool)
	} else {
		repo = NewApplicationRepository(sink)
		docs = NewDocumentStore()
		liveness = NewLivenessStore()
	}

	screeningStore := NewScreeningStore()
	screeningClient := NewInMemoryScreeningClient()
	screening := NewScreeningService(screeningClient, repo, screeningStore, audit)
	vendor, err := NewVendorClient()
	if err != nil {
		panic(err)
	}
	webhook := NewWebhookService(NewWebhookStore(), repo, audit, vendor)
	rekyc := NewReKYCService(repo, audit)
	return &Services{
		Repo:     repo,
		Docs:     docs,
		Liveness: liveness,
		Screen:   screening,
		Webhook:  webhook,
		Audit:    audit,
		Vendor:   vendor,
		ReKYC:    rekyc,
	}
}

// newMux builds the HTTP router.
func newMux() *http.ServeMux {
	return newMuxWithServices(newServices())
}

// newMuxWithServices builds the HTTP router with injected services.
func newMuxWithServices(s *Services) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandler(s))
	mux.HandleFunc("/metrics", metricsHandler)
	mux.HandleFunc("POST /v1/kyc/applications", createApplicationHandler(s))
	mux.HandleFunc("GET /v1/kyc/applications/{id}", getApplicationHandler(s))
	mux.HandleFunc("GET /v1/kyc/status/{user_id}", getStatusHandler(s))
	mux.HandleFunc("POST /v1/kyc/applications/{id}/documents", uploadDocumentHandler(s))
	mux.HandleFunc("GET /v1/kyc/applications/{id}/documents", listDocumentsHandler(s))
	mux.HandleFunc("POST /v1/kyc/applications/{id}/liveness", startLivenessHandler(s))
	mux.HandleFunc("GET /v1/kyc/applications/{id}/liveness", getLivenessHandler(s))
	mux.HandleFunc("POST /v1/kyc/applications/{id}/screening", runScreeningHandler(s))
	mux.HandleFunc("POST /v1/kyc/applications/{id}/screening/disposition", screeningDispositionHandler(s))
	mux.HandleFunc("POST /v1/webhooks/{vendor}", webhookHandler(s))
	mux.HandleFunc("POST /internal/v1/rekyc/trigger", triggerReKYCHandler(s))
	mux.HandleFunc("GET /v1/audit-events", auditEventsHandler(s))
	return mux
}

// newServer wires middleware and returns an *http.Server.
func newServer(s *Services, addr string) *http.Server {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	handler := correlationMiddleware(spanMiddleware(loggingMiddleware(logger, newMuxWithServices(s))))
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func Run(addr string) error {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if _, err := installTracer(ctx, logger); err != nil {
		return fmt.Errorf("install tracer: %w", err)
	}
	defer shutdownTracer(context.Background())
	srv := newServer(newServices(), addr)
	return srv.ListenAndServe()
}

// --- Handlers ---

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func readyzHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}

type createAppRequest struct {
	UserID   string `json:"user_id"`
	Vendor   string `json:"vendor,omitempty"`
	FullName string `json:"full_name,omitempty"`
}

type createAppResponse struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	State     State     `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

func createApplicationHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createAppRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, r, err)
			return
		}
		if req.UserID == "" {
			writeError(w, r, errMissingUserID)
			return
		}
		app := &Application{
			ID:     newUUID(),
			UserID: req.UserID,
			Vendor: req.Vendor,
			State:  StateStarted,
		}
		if err := s.Repo.Create(app); err != nil {
			writeError(w, r, err)
			return
		}
		// Register with vendor asynchronously-ish (synchronously here, stub).
		if s.Vendor != nil {
			vid, vErr := s.Vendor.CreateApplicant(r.Context(), VendorApplicant{
				ApplicationID: app.ID,
				UserID:         app.UserID,
				FullName:      req.FullName,
			})
			if vErr == nil {
				globalMetrics.vendorCallsTotal.Add(1)
				// store vendor applicant id via repo update (locked). We mutate
				// the app pointer directly because it lives in the repo map.
				s.Repo.SetVendorApplicantID(app.ID, vid)
				if s.Audit != nil {
					s.Audit.Record(app.ID, "vendor_create_applicant", "system", map[string]any{
						"vendor":          s.Vendor.Name(),
						"vendor_applicant_id": vid,
					})
				}
			}
		}
		s.Audit.Record(app.ID, "application_created", "system", map[string]any{
			"user_id": app.UserID,
		})
		globalMetrics.createAppTotal.Add(1)
		writeJSON(w, http.StatusCreated, createAppResponse{
			ID:        app.ID,
			UserID:    app.UserID,
			State:     app.State,
			CreatedAt: app.CreatedAt,
		})
	}
}

type applicationView struct {
	Application
	Documents      []Document        `json:"documents"`
	Liveness       *LivenessSession  `json:"liveness,omitempty"`
	SanctionsHits   []SanctionsHit    `json:"sanctions_hits,omitempty"`
}

func getApplicationHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		app, err := s.Repo.Get(id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		view := applicationView{Application: *app}
		view.Documents = s.Docs.List(id)
		if sess, ok := s.Liveness.Latest(id); ok {
			view.Liveness = &sess
		}
		writeJSON(w, http.StatusOK, view)
	}
}

type statusResponse struct {
	UserID          string     `json:"user_id"`
	ApplicationID   string     `json:"application_id"`
	State           State      `json:"state"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	ReKYCDueAt      *time.Time `json:"re_kyc_due_at,omitempty"`
}

func getStatusHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := r.PathValue("user_id")
		app, err := s.Repo.GetByUserID(uid)
		if err != nil {
			writeError(w, r, err)
			return
		}
		resp := statusResponse{
			UserID:        app.UserID,
			ApplicationID: app.ID,
			State:         app.State,
			UpdatedAt:     app.UpdatedAt,
		}
		if !app.DecidedAt.IsZero() {
			t := app.DecidedAt
			resp.DecidedAt = &t
		}
		if !app.ReKYCDueAt.IsZero() {
			t := app.ReKYCDueAt
			resp.ReKYCDueAt = &t
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

var validDocTypes = map[string]bool{
	"id_front": true,
	"id_back":  true,
	"selfie":   true,
	"poa":      true,
}

type uploadDocJSONRequest struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

func uploadDocumentHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		app, err := s.Repo.Get(id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		docType, content, perr := parseDocRequest(r)
		if perr != nil {
			writeError(w, r, perr)
			return
		}
		if !validDocTypes[docType] {
			writeError(w, r, errBadDocType)
			return
		}
		now := time.Now()
		doc := Document{
			ID:             newUUID(),
			ApplicationID:  id,
			Type:           docType,
			Content:        content,
			UploadedAt:     now,
			RetentionUntil: now.Add(365 * 24 * time.Hour),
		}
		s.Docs.Add(id, doc)
		globalMetrics.uploadDocTotal.Add(1)
		if s.Vendor != nil && app.VendorApplicantID != "" {
			if vdocID, vErr := s.Vendor.UploadDocument(r.Context(), app.VendorApplicantID, VendorDocument{
				Type:    docType,
				Content: content,
			}); vErr == nil {
				globalMetrics.vendorCallsTotal.Add(1)
				s.Audit.Record(id, "vendor_upload_document", "system", map[string]any{
					"vendor_document_id": vdocID,
				})
			}
		}
		// Transition to documents_uploaded if required docs satisfied and
		// current state is started.
		if app.State == StateStarted && s.Docs.HasRequiredDocs(id) {
			if _, uerr := s.Repo.UpdateState(id, app.Version, StateDocumentsUploaded, "system", "required documents uploaded"); uerr != nil {
				// best-effort; transition may fail on conflict — still return 201
			}
		}
		s.Audit.Record(id, "document_uploaded", "system", map[string]any{"type": docType})
		writeJSON(w, http.StatusCreated, doc)
	}
}

// parseDocRequest parses either multipart/form-data or a JSON body.
func parseDocRequest(r *http.Request) (docType string, content []byte, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		if perr := r.ParseMultipartForm(10 << 20); perr != nil {
			return "", nil, NewAppError("bad_form", "invalid multipart form", http.StatusBadRequest)
		}
		docType = r.FormValue("type")
		f, fh, ferr := r.FormFile("file")
		if ferr == nil {
			defer f.Close()
			content, _ = io.ReadAll(f)
			_ = fh
		} else if c := r.FormValue("content"); c != "" {
			content = []byte(c)
		}
		return docType, content, nil
	}
	var req uploadDocJSONRequest
	if derr := decodeJSON(r, &req); derr != nil {
		return "", nil, derr
	}
	return req.Type, []byte(req.Content), nil
}

func listDocumentsHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.Repo.Get(id); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"documents": s.Docs.List(id)})
	}
}

func startLivenessHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		app, err := s.Repo.Get(id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if app.State != StateDocumentsUploaded && app.State != StateStarted {
			writeError(w, r, NewAppError("bad_state", "liveness requires documents_uploaded or started state", http.StatusConflict))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		vendorSessionID, vErr := s.Vendor.StartLiveness(ctx, app.VendorApplicantID)
		if vErr != nil {
			writeError(w, r, NewAppError("vendor_error", "vendor liveness start failed: "+vErr.Error(), http.StatusBadGateway))
			return
		}
		now := time.Now()
		sess := LivenessSession{
			ID:              newUUID(),
			ApplicationID:   id,
			VendorSessionID: vendorSessionID,
			Status:          "passed",
			StartedAt:       now,
			CompletedAt:     now,
			Result:          "pass",
			RetentionUntil:  now.Add(365 * 24 * time.Hour),
		}
		s.Liveness.Add(id, sess)
		globalMetrics.livenessTotal.Add(1)
		s.Audit.Record(id, "liveness_started", "system", map[string]any{
			"vendor_session_id": vendorSessionID,
		})
		// On pass, transition documents_uploaded -> liveness_passed.
		if sess.Result == "pass" {
			to := StateLivenessPassed
			if app.State != StateDocumentsUploaded {
				// also allow from started directly for stub convenience
				to = StateLivenessPassed
			}
			if _, uerr := s.Repo.UpdateState(id, app.Version, to, "system", "liveness passed"); uerr != nil {
				// best effort
			}
			s.Audit.Record(id, "liveness_passed", "system", nil)
		}
		writeJSON(w, http.StatusCreated, sess)
	}
}

func getLivenessHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.Repo.Get(id); err != nil {
			writeError(w, r, err)
			return
		}
		sess, ok := s.Liveness.Latest(id)
		if !ok {
			writeError(w, r, NewAppError("no_liveness", "no liveness session", http.StatusNotFound))
			return
		}
		writeJSON(w, http.StatusOK, sess)
	}
}

type runScreeningRequest struct {
	FullName string `json:"full_name"`
}

func runScreeningHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		app, err := s.Repo.Get(id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		var req runScreeningRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, r, err)
			return
		}
		hits, manual, serr := s.Screen.Run(r.Context(), app, req.FullName)
		if serr != nil {
			writeError(w, r, NewAppError("screening_error", serr.Error(), http.StatusInternalServerError))
			return
		}
		globalMetrics.screeningTotal.Add(1)
		globalMetrics.screeningHits.Add(int64(len(hits)))
		var to State
		if manual {
			to = StateManualReview
		} else {
			to = StateScreening
		}
		if CanTransition(app.State, to) {
			if _, uerr := s.Repo.UpdateState(id, app.Version, to, "system", "screening run"); uerr != nil {
				// best effort
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"hits":          hits,
			"manual_review":  manual,
			"state":          app.State,
		})
	}
}

type dispositionRequest struct {
	Disposition string `json:"disposition"`
	ReviewedBy  string `json:"reviewed_by"`
}

func screeningDispositionHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req dispositionRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, r, err)
			return
		}
		if err := s.Screen.Disposition(r.Context(), id, req.Disposition, req.ReviewedBy); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func webhookHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vendor := r.PathValue("vendor")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, r, NewAppError("bad_body", "cannot read body", http.StatusBadRequest))
			return
		}
		sig := r.Header.Get("X-Webhook-Signature")
		if sig == "" {
			sig = r.Header.Get("X-Signature")
		}
		ts := r.Header.Get("X-Webhook-Timestamp")
		if ts == "" {
			ts = r.Header.Get("X-Timestamp")
		}
		eventID := r.Header.Get("X-Webhook-Id")
		if eventID == "" {
			eventID = r.Header.Get("X-Event-Id")
		}
		res := s.Webhook.Ingest(r.Context(), vendor, raw, sig, ts, eventID)
		switch {
		case res.Accepted:
			globalMetrics.webhookAccept.Add(1)
			writeJSON(w, http.StatusOK, map[string]string{"status": "accepted", "event_id": res.EventID})
		case res.Duplicate:
			globalMetrics.webhookDuplicate.Add(1)
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "event_id": res.EventID})
		default:
			globalMetrics.webhookReject.Add(1)
			writeError(w, r, NewAppError("webhook_rejected", res.Reason, http.StatusUnauthorized))
		}
	}
}

type triggerReKYCRequest struct {
	UserID string `json:"user_id"`
}

func triggerReKYCHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req triggerReKYCRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, r, err)
			return
		}
		if req.UserID == "" {
			writeError(w, r, NewAppError("missing_user_id", "user_id is required", http.StatusBadRequest))
			return
		}
		app, err := s.Repo.GetByUserID(req.UserID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if _, err := s.Repo.Reopen(app.ID, app.Version, "internal"); err != nil {
			writeError(w, r, err)
			return
		}
		s.Audit.Record(app.ID, "re_kyc_triggered", "internal", map[string]any{"user_id": req.UserID})
		globalMetrics.rekycTotal.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{"status": "reopened"})
	}
}

func auditEventsHandler(s *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"events": s.Audit.List()})
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return errBadJSON
	}
	if len(body) == 0 {
		return errBadJSON
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errBadJSON
	}
	return nil
}

// ReKYCService runs the scheduler tick and manual triggers.
type ReKYCService struct {
	repo  ApplicationRepo
	audit *AuditLog
	mu    sync.Mutex
	stop  chan struct{}
}

// NewReKYCService creates a new ReKYCService.
func NewReKYCService(repo ApplicationRepo, audit *AuditLog) *ReKYCService {
	return &ReKYCService{repo: repo, audit: audit, stop: make(chan struct{})}
}

// Tick runs one scheduler pass, re-opening any due terminal applications.
func (r *ReKYCService) Tick(now time.Time) int {
	due := r.repo.ListDueForReKYC(now)
	reopened := 0
	for _, d := range due {
		if _, err := r.repo.Reopen(d.ID, d.Version, "scheduler"); err == nil {
			reopened++
			if r.audit != nil {
				r.audit.Record(d.ID, "re_kyc_scheduled", "scheduler", nil)
			}
		}
	}
	return reopened
}

// Start launches a background goroutine ticking every interval.
func (r *ReKYCService) Start(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.Tick(time.Now())
			}
		}
	}()
}

// Stop terminates the background scheduler.
func (r *ReKYCService) Stop() {
	close(r.stop)
}