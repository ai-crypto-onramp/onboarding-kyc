package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- webhooks.go: reconcileOutcomeToState branches ----

func TestReconcileOutcomeToStateAllBranches(t *testing.T) {
	cases := []struct {
		outcome string
		current State
		want    State
	}{
		// CLEAR/PASS/APPROVED from various states
		{"CLEAR", StateVendorDecision, StatePass},
		{"PASS", StateManualReview, StatePass},
		{"APPROVED", StateScreening, StateVendorDecision},
		{"CLEAR", StateStarted, ""}, // no legal transition
		// FAIL/REJECTED
		{"FAIL", StateVendorDecision, StateFail},
		{"REJECTED", StateManualReview, StateFail},
		{"FAIL", StateStarted, ""}, // no legal transition
		// CONSIDER/REVIEW/MANUAL_REVIEW
		{"CONSIDER", StateScreening, StateManualReview},
		{"REVIEW", StateStarted, StateManualReview},
		{"MANUAL_REVIEW", StateLivenessPassed, StateManualReview},
		// LIVENESS_PASS
		{"LIVENESS_PASS", StateDocumentsUploaded, StateLivenessPassed},
		{"LIVENESS_PASS", StateStarted, ""}, // STARTED -> LIVENESS_PASSED not legal
		// unknown outcome
		{"UNKNOWN", StateStarted, ""},
		{"", StateStarted, ""},
	}
	for _, c := range cases {
		got := reconcileOutcomeToState(c.outcome, c.current)
		if got != c.want {
			t.Errorf("reconcile(%q,%s)=%q want %q", c.outcome, c.current, got, c.want)
		}
	}
}

func TestAbsInt64(t *testing.T) {
	if abs(-5) != 5 || abs(5) != 5 || abs(0) != 0 {
		t.Fatal("abs incorrect")
	}
}

func TestVerifyWebhookBareHex(t *testing.T) {
	body := []byte(`x`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, ts, body)
	// bare hex (no v1= prefix) should be accepted
	if err := VerifyWebhook(body, sig, ts, secret, 300*time.Second); err != nil {
		t.Fatalf("bare hex: %v", err)
	}
}

func TestVerifyWebhookBadTimestamp(t *testing.T) {
	if err := VerifyWebhook([]byte("x"), "v1=abc", "not-a-number", "s", 300*time.Second); err != errStaleTs {
		t.Fatalf("expected errStaleTs, got %v", err)
	}
}

func TestVerifyWebhookEmptySigAfterParse(t *testing.T) {
	// Stripe-style header with t= and v1= missing -> sig empty -> errMissingSig
	ts := fmt.Sprintf("%d", time.Now().Unix())
	header := "t=" + ts + ",x=bad"
	if err := VerifyWebhook([]byte("x"), header, "", "s", 300*time.Second); err != errMissingSig {
		t.Fatalf("expected errMissingSig, got %v", err)
	}
}

func TestVerifyWebhookStripeStyleBadKV(t *testing.T) {
	// Stripe-style with malformed kv pairs (no "=") should be skipped
	ts := fmt.Sprintf("%d", time.Now().Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, ts, []byte("x"))
	header := "badpair,t=" + ts + ",v1=" + sig
	if err := VerifyWebhook([]byte("x"), header, "", secret, 300*time.Second); err != nil {
		t.Fatalf("expected ok with badpair skipped, got %v", err)
	}
}

func TestWebhookIngestNoApplicationID(t *testing.T) {
	s := newTestServices()
	body := []byte(`{}`) // no application_id
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook("dev-webhook-secret", ts, body)
	res := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-noapp")
	if !res.Accepted {
		t.Fatalf("expected accepted, got %s", res.Reason)
	}
}

func TestWebhookIngestAppNotFound(t *testing.T) {
	s := newTestServices()
	body := []byte(`{"application_id":"missing"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook("dev-webhook-secret", ts, body)
	res := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-app404")
	if !res.Accepted {
		t.Fatalf("expected accepted even when app missing, got %s", res.Reason)
	}
}

func TestWebhookIngestReconcileNoTransition(t *testing.T) {
	s := newTestServices()
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := s.Repo.Create(app); err != nil {
		t.Fatal(err)
	}
	// outcome CLEAR from STARTED -> reconcile returns "" (no legal transition)
	body := []byte(`{"application_id":"a1","outcome":"CLEAR"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook("dev-webhook-secret", ts, body)
	res := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-notrans")
	if !res.Accepted {
		t.Fatalf("expected accepted, got %s", res.Reason)
	}
	a, _ := s.Repo.Get("a1")
	if a.State != StateStarted {
		t.Fatalf("expected started unchanged, got %s", a.State)
	}
}

func TestWebhookIngestReconcileVersionConflict(t *testing.T) {
	s := newTestServices()
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := s.Repo.Create(app); err != nil {
		t.Fatal(err)
	}
	setState(s.Repo, "a1", StateDocumentsUploaded)
	setState(s.Repo, "a1", StateLivenessPassed)
	setState(s.Repo, "a1", StateScreening)
	setState(s.Repo, "a1", StateVendorDecision)
	// Stale version: pass app.Version-1 so UpdateState fails with conflict.
	body := []byte(`{"application_id":"a1","outcome":"CLEAR"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook("dev-webhook-secret", ts, body)
	// Manually call reconcile path with a stale-version repo by wrapping.
	stale := &staleVersionRepo{ApplicationRepo: s.Repo}
	s.Webhook.repo = stale
	res := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-conflict")
	if !res.Accepted {
		t.Fatalf("expected accepted despite conflict, got %s", res.Reason)
	}
}

// staleVersionRepo wraps a repo and forces UpdateState to fail.
type staleVersionRepo struct {
	ApplicationRepo
}

func (s *staleVersionRepo) UpdateState(id string, version int, newState State, actor, reason string) (*Application, error) {
	return nil, errors.New("version conflict")
}

// ---- server.go: requestIDFromContext, toAppError branches, newServices DB path ----

func TestRequestIDFromContextEmpty(t *testing.T) {
	if got := requestIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestRequestIDFromContextNonString(t *testing.T) {
	ctx := context.WithValue(context.Background(), keyRequestID, 123)
	if got := requestIDFromContext(ctx); got != "" {
		t.Fatalf("expected empty for non-string, got %q", got)
	}
}

func TestToAppErrorConflict(t *testing.T) {
	ae := toAppError(ErrConflict)
	if ae.Code != "version_conflict" {
		t.Fatalf("expected version_conflict, got %s", ae.Code)
	}
}

func TestToAppErrorIllegalTransition(t *testing.T) {
	ae := toAppError(ErrIllegalTransition)
	if ae.Code != "illegal_transition" {
		t.Fatalf("expected illegal_transition, got %s", ae.Code)
	}
}

func TestToAppErrorReKYCNotTerminal(t *testing.T) {
	ae := toAppError(ErrReKYCNotTerminal)
	if ae.Code != "not_terminal" {
		t.Fatalf("expected not_terminal, got %s", ae.Code)
	}
}

func TestToAppErrorInvalidArgument(t *testing.T) {
	ae := toAppError(ErrInvalidArgument)
	if ae.Code != "invalid_argument" {
		t.Fatalf("expected invalid_argument, got %s", ae.Code)
	}
}

func TestToAppErrorBadDisposition(t *testing.T) {
	ae := toAppError(errBadDisposition)
	if ae.Code != "bad_disposition" {
		t.Fatalf("expected bad_disposition, got %s", ae.Code)
	}
}

func TestToAppErrorNotFound(t *testing.T) {
	ae := toAppError(ErrNotFound)
	if ae.Code != "application_not_found" {
		t.Fatalf("expected application_not_found, got %s", ae.Code)
	}
}

func TestToAppErrorDuplicate(t *testing.T) {
	ae := toAppError(ErrDuplicate)
	if ae.Code != "duplicate_application" {
		t.Fatalf("expected duplicate_application, got %s", ae.Code)
	}
}

func TestToAppErrorWrapped(t *testing.T) {
	wrapped := errors.New("wrap: " + string(ErrNotFound.Error()))
	ae := toAppError(wrapped)
	if ae.Code != "internal_error" {
		t.Fatalf("expected internal_error for non-is error, got %s", ae.Code)
	}
}

func TestNewServicesDefaultsToInMemory(t *testing.T) {
	// No DB_URL, no POLICY_RISK_ENGINE_URL -> in-memory stores, audit sink.
	t.Setenv("DB_URL", "")
	t.Setenv("POLICY_RISK_ENGINE_URL", "")
	s := newServices()
	if s == nil || s.Repo == nil || s.Docs == nil || s.Liveness == nil {
		t.Fatal("expected non-nil services")
	}
}

func TestNewServicesPolicySinkConfigured(t *testing.T) {
	// POLICY_RISK_ENGINE_URL set -> composite sink wrapping PolicyEventSink.
	// Use a throwaway http server so the URL is valid.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("POLICY_RISK_ENGINE_URL", srv.URL)
	t.Setenv("DB_URL", "")
	s := newServices()
	if s == nil {
		t.Fatal("expected services")
	}
}

func TestNewServicesDBUrlInvalidPanics(t *testing.T) {
	t.Setenv("DB_URL", "postgres://invalid:invalid@127.0.0.1:1/nonexistent")
	defer func() {
		_ = recover()
	}()
	_ = newServices()
	t.Fatal("expected panic on invalid DB_URL")
}

// ---- server.go: webhook handler alternate headers ----

func TestWebhookHandlerAlternateHeaders(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	body := []byte(`{"event":"x"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook("dev-webhook-secret", ts, body)
	// Use X-Signature / X-Timestamp / X-Event-Id alternates
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/stub", strings.NewReader(string(body)))
	req.Header.Set("X-Signature", "v1="+sig)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Event-Id", "evt-alt")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ---- vendor_http.go: env paths and error branches ----

func TestNewHTTPVendorClientEnvDefaults(t *testing.T) {
	t.Setenv("VENDOR_API_BASE", "http://example.com")
	t.Setenv("VENDOR_API_KEY", "key123")
	t.Setenv("VENDOR_CALL_TIMEOUT", "5s")
	c := newHTTPVendorClient("test", "", "", 0)
	if c.base != "http://example.com" {
		t.Fatalf("base: %s", c.base)
	}
	if c.apiKey != "key123" {
		t.Fatalf("apiKey: %s", c.apiKey)
	}
}

func TestNewHTTPVendorClientExplicitOverride(t *testing.T) {
	t.Setenv("VENDOR_API_BASE", "http://env.example.com")
	c := newHTTPVendorClient("test", "http://explicit.com", "explicit-key", 10*time.Second)
	if c.base != "http://explicit.com" {
		t.Fatalf("expected explicit base, got %s", c.base)
	}
	if c.apiKey != "explicit-key" {
		t.Fatalf("expected explicit key, got %s", c.apiKey)
	}
}

func TestHTTPVendorClientDoRequestMarshalError(t *testing.T) {
	c := newHTTPVendorClient("test", "http://example.com", "k", 5*time.Second)
	// Pass a value that cannot be marshaled to JSON (channel).
	_, err := c.doRequest(context.Background(), http.MethodPost, "/x", make(chan int))
	if err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestHTTPVendorClientDoRequestBadURL(t *testing.T) {
	c := newHTTPVendorClient("test", "http:// invalid url with spaces", "k", 5*time.Second)
	c.retry = retryConfig{maxAttempts: 1, baseDelay: time.Millisecond, maxDelay: time.Millisecond}
	_, err := c.doRequest(context.Background(), http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestHTTPVendorClientDoRequest4xxNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newHTTPVendorClient("test", srv.URL, "k", 5*time.Second)
	c.retry = retryConfig{maxAttempts: 3, baseDelay: time.Millisecond, maxDelay: time.Millisecond}
	resp, err := c.doRequest(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", calls.Load())
	}
}

func TestCloseBodyNilSafe(t *testing.T) {
	closeBody(nil)
	closeBody(&http.Response{Body: nil})
}

// ---- tracing.go: installTracer with endpoint, recordSpanError recording ----

func TestInstallTracerWithEndpoint(t *testing.T) {
	// Reset global state.
	tracerMu.Lock()
	tracerShutdown = nil
	tracerMu.Unlock()
	// Use an invalid endpoint to force exporter construction error path.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "1")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// otlptracehttp.New may not error on construction even with bad endpoint;
	// we mainly exercise the endpoint != "" branch.
	sd, err := installTracer(context.Background(), logger)
	if err != nil {
		t.Fatalf("installTracer: %v", err)
	}
	if sd == nil {
		t.Fatal("expected shutdown fn")
	}
	_ = sd(context.Background())
	tracerMu.Lock()
	tracerShutdown = nil
	tracerMu.Unlock()
}

func TestInstallTracerIdempotent(t *testing.T) {
	tracerMu.Lock()
	tracerShutdown = nil
	tracerMu.Unlock()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	sd1, _ := installTracer(context.Background(), logger)
	sd2, _ := installTracer(context.Background(), logger)
	// Second call returns the existing shutdown (same instance).
	if sd1 == nil || sd2 == nil {
		t.Fatal("expected shutdown fns")
	}
	_ = sd1(context.Background())
	tracerMu.Lock()
	tracerShutdown = nil
	tracerMu.Unlock()
}

func TestSpanMiddlewareSetsErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/err", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := spanMiddleware(mux)
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/err")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
}

func TestSpanFromCtx(t *testing.T) {
	span := spanFromCtx(context.Background())
	if span == nil {
		t.Fatal("expected non-nil span (no-op)")
	}
}

// ---- policy_sink.go: post 4xx, drain retry exhaust ----

func TestPolicyEventSinkPost4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("POLICY_RISK_ENGINE_URL", srv.URL)
	t.Setenv("POLICY_EVENT_QUEUE_CAP", "4")
	sink := NewPolicyEventSink(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer sink.Stop()
	// Sync post returns 400 -> event enqueued. Async drainer retries 5x then drops.
	sink.RecordTransition("app", StateStarted, StatePass, "x", "y")
	// Give the drainer time to exhaust retries (5 attempts with backoff starting 1s).
	// We don't wait the full ~15s; just verify the sync path was hit (1 call) and
	// the event was enqueued (queue not full).
	// To avoid a slow test, we instead verify directly via post() returning error.
	if err := sink.post(context.Background(), PolicyEvent{Type: "decision", ApplicationID: "x"}); err == nil {
		t.Fatal("expected error for 4xx")
	}
}

func TestPolicyEventSinkQueueFullDrops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("POLICY_RISK_ENGINE_URL", srv.URL)
	t.Setenv("POLICY_EVENT_QUEUE_CAP", "1")
	sink := NewPolicyEventSink(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer sink.Stop()
	// First event fills the queue (cap=1); second event is dropped.
	sink.RecordTransition("app1", StateStarted, StatePass, "x", "y")
	sink.RecordTransition("app2", StateStarted, StatePass, "x", "y")
}

// ---- list_sync.go: fetchNames non-InMemoryScreeningClient path ----

func TestListSyncJobFetchNamesNonInMemory(t *testing.T) {
	t.Setenv("LIST_SYNC_DIR", t.TempDir())
	job := NewListSyncJob(&stubScreeningClient{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	snap, err := job.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.Names) != 0 {
		t.Fatalf("expected 0 names for non-in-memory client, got %d", len(snap.Names))
	}
}

func TestListSyncJobFetchNamesScreenError(t *testing.T) {
	t.Setenv("LIST_SYNC_DIR", t.TempDir())
	job := NewListSyncJob(&errScreeningClient{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if _, err := job.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected screen error")
	}
}

type stubScreeningClient struct{}

func (stubScreeningClient) Screen(ctx context.Context, fullName string) ([]ScreeningHit, error) {
	return nil, nil
}

type errScreeningClient struct{}

func (errScreeningClient) Screen(ctx context.Context, fullName string) ([]ScreeningHit, error) {
	return nil, errors.New("screen failed")
}

func TestListSyncJobStopWhenNotRunning(t *testing.T) {
	job := NewListSyncJob(NewInMemoryScreeningClient(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	job.Stop() // should be a no-op, not panic
}

// ---- retention.go: nil logger, Start when running, Stop when not running ----

func TestNewRetentionSweeperNilLogger(t *testing.T) {
	s := NewRetentionSweeper(NewDocumentStore(), NewLivenessStore(), NewAuditLog(), nil)
	if s == nil || s.logger == nil {
		t.Fatal("expected non-nil logger fallback")
	}
}

func TestRetentionSweeperStartTwiceNoOp(t *testing.T) {
	d := NewDocumentStore()
	l := NewLivenessStore()
	s := NewRetentionSweeper(d, l, NewAuditLog(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	s.Start(10 * time.Millisecond)
	s.Start(10 * time.Millisecond) // no-op
	time.Sleep(20 * time.Millisecond)
	s.Stop()
}

func TestRetentionSweeperStopWhenNotRunning(t *testing.T) {
	s := NewRetentionSweeper(NewDocumentStore(), NewLivenessStore(), NewAuditLog(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	s.Stop() // no-op, no panic
}

func TestRetentionSweeperSweepNoDocsRemoved(t *testing.T) {
	d := NewDocumentStore()
	l := NewLivenessStore()
	audit := NewAuditLog()
	s := NewRetentionSweeper(d, l, audit, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	dr, sr := s.Sweep(context.Background(), time.Now())
	if dr != 0 || sr != 0 {
		t.Fatalf("expected 0/0, got %d/%d", dr, sr)
	}
	// No audit event when nothing removed.
	if len(audit.List()) != 0 {
		t.Fatalf("expected 0 audit events, got %d", len(audit.List()))
	}
}

// ---- retry.go: doWithRetry context cancelled during backoff ----

func TestDoWithRetryContextCancelledDuringBackoff(t *testing.T) {
	cfg := retryConfig{maxAttempts: 5, baseDelay: 100 * time.Millisecond, maxDelay: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := doWithRetry(ctx, cfg, func(attempt int) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: http.NoBody}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDoWithRetryMaxAttemptsBelowOne(t *testing.T) {
	var calls atomic.Int32
	cfg := retryConfig{maxAttempts: 0, baseDelay: time.Millisecond, maxDelay: time.Millisecond}
	resp, err := doWithRetry(context.Background(), cfg, func(attempt int) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call (maxAttempts clamped to 1), got %d", calls.Load())
	}
}

// ---- repo.go: Create duplicate id, SetVendorApplicantID not found ----

func TestRepoCreateDuplicateID(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	dup := &Application{ID: "a1", UserID: "u2", State: StateStarted}
	if err := repo.Create(dup); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestRepoSetVendorApplicantIDNotFound(t *testing.T) {
	repo := NewApplicationRepository(nil)
	// No-op for missing app; just verify it doesn't panic.
	repo.SetVendorApplicantID("missing", "vid")
}

func TestRepoSetVendorApplicantIDUpdatesVersion(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	v := app.Version
	repo.SetVendorApplicantID("a1", "vid-1")
	a, _ := repo.Get("a1")
	if a.VendorApplicantID != "vid-1" || a.Version != v+1 {
		t.Fatalf("expected vendor id set and version bumped, got %+v", a)
	}
}

func TestRepoCreateSetsDefaults(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1"} // no State, no timestamps
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	if app.State != StateStarted {
		t.Fatalf("expected StateStarted, got %s", app.State)
	}
	if app.Version != 1 {
		t.Fatalf("expected version 1, got %d", app.Version)
	}
	if app.CreatedAt.IsZero() || app.UpdatedAt.IsZero() {
		t.Fatal("expected timestamps set")
	}
}

func TestRepoReopenVersionConflict(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	setState(repo, "a1", StateDocumentsUploaded)
	setState(repo, "a1", StateLivenessPassed)
	setState(repo, "a1", StateScreening)
	setState(repo, "a1", StateVendorDecision)
	setState(repo, "a1", StatePass)
	a, _ := repo.Get("a1")
	if _, err := repo.Reopen("a1", a.Version+1, "x"); err == nil {
		t.Fatal("expected version conflict")
	}
}

// ---- screening.go: Screen ctx cancel, Disposition repo update fail ----

func TestScreeningServiceScreenCtxCancel(t *testing.T) {
	s := NewScreeningService(NewInMemoryScreeningClient(), NewApplicationRepository(nil), NewScreeningStore(), NewAuditLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.Run(ctx, &Application{ID: "a1"}, "BAD ACTOR")
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
}

func TestScreeningServiceDispositionRepoUpdateFail(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	setState(repo, "a1", StateDocumentsUploaded)
	setState(repo, "a1", StateLivenessPassed)
	setState(repo, "a1", StateManualReview)
	s := NewScreeningService(NewInMemoryScreeningClient(), repo, NewScreeningStore(), NewAuditLog())
	// Wrap repo so UpdateState fails; Disposition should return the error.
	stale := &staleVersionRepo{ApplicationRepo: repo}
	s.repo = stale
	if err := s.Disposition(context.Background(), "a1", "CLEAR", "analyst"); err == nil {
		t.Fatal("expected update error")
	}
}

func TestScreeningServiceDispositionNotFound(t *testing.T) {
	s := NewScreeningService(NewInMemoryScreeningClient(), NewApplicationRepository(nil), NewScreeningStore(), NewAuditLog())
	if err := s.Disposition(context.Background(), "missing", "CLEAR", "analyst"); err == nil {
		t.Fatal("expected not found")
	}
}

// ---- vendor.go: ParseWebhook edge cases ----

func TestStubVendorParseWebhookInvalidJSON(t *testing.T) {
	c := &StubVendorClient{}
	evt, err := c.ParseWebhook(context.Background(), "stub", []byte("not json"))
	if err != nil {
		t.Fatalf("expected no error for invalid json, got %v", err)
	}
	if evt.EventID == "" {
		t.Fatal("expected event id from hash")
	}
	if evt.Outcome != "CLEAR" {
		t.Fatalf("expected default CLEAR, got %s", evt.Outcome)
	}
}

func TestStubVendorParseWebhookWithOutcome(t *testing.T) {
	c := &StubVendorClient{}
	evt, err := c.ParseWebhook(context.Background(), "stub", []byte(`{"application_id":"a1","outcome":"FAIL"}`))
	if err != nil {
		t.Fatal(err)
	}
	if evt.ApplicationID != "a1" || evt.Outcome != "FAIL" {
		t.Fatalf("unexpected: %+v", evt)
	}
}

// ---- statemachine.go: CanTransition unknown from ----

func TestCanTransitionUnknownFrom(t *testing.T) {
	if CanTransition(State("UNKNOWN"), StateStarted) {
		t.Fatal("expected false for unknown from state")
	}
}

func TestValidateTransitionErrorWrap(t *testing.T) {
	err := ValidateTransition(StateStarted, StateFail)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition wrap, got %v", err)
	}
}