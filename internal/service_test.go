package internal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers ----

func newTestServices() *Services {
	audit := NewAuditLog()
	repo := NewApplicationRepository(audit)
	docs := NewDocumentStore()
	liveness := NewLivenessStore()
	screeningStore := NewScreeningStore()
	screeningClient := NewInMemoryScreeningClient()
	screening := NewScreeningService(screeningClient, repo, screeningStore, audit)
	vendor, _ := NewVendorClient()
	webhook := NewWebhookService(NewWebhookStore(), repo, audit, vendor)
	rekyc := NewReKYCService(repo, audit)
	return &Services{
		Repo: repo, Docs: docs, Liveness: liveness,
		Screen: screening, Webhook: webhook, Audit: audit,
		Vendor: vendor, ReKYC: rekyc,
	}
}

func newTestServer(s *Services) *httptest.Server {
	mux := newMuxWithServices(s)
	// Apply the same correlation+logging middleware the real server uses so
	// tests see request_id propagation and headers.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler := correlationMiddleware(loggingMiddleware(logger, mux))
	return httptest.NewServer(handler)
}

func doJSON(t *testing.T, c *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("newreq: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func createApp(t *testing.T, srv *httptest.Server, userID string) string {
	t.Helper()
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications", map[string]string{
		"user_id":   userID,
		"full_name": "Good Person",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create app: status %d body %s", resp.StatusCode, b)
	}
	var out struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &out)
	return out.ID
}

func uploadDoc(t *testing.T, srv *httptest.Server, appID, docType string) {
	t.Helper()
	resp := doJSON(t, srv.Client(), http.MethodPost,
		srv.URL+"/v1/kyc/applications/"+appID+"/documents",
		map[string]string{"type": docType, "content": "data"})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("upload doc %s: status %d body %s", docType, resp.StatusCode, b)
	}
	resp.Body.Close()
}

func setState(repo ApplicationRepo, id string, to State) {
	app, _ := repo.Get(id)
	_, _ = repo.UpdateState(id, app.Version, to, "test", "test")
}

// ---- State machine tests ----

func TestLegalTransitions(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}

	type tc struct{ from, to State }
	cases := []tc{
		{StateStarted, StateDocumentsUploaded},
		{StateDocumentsUploaded, StateLivenessPassed},
		{StateLivenessPassed, StateScreening},
		{StateScreening, StateVendorDecision},
		{StateVendorDecision, StatePass},
		{StateVendorDecision, StateFail},
		{StateVendorDecision, StateManualReview},
		{StateManualReview, StatePass},
		{StateManualReview, StateFail},
		{StatePass, StateStarted}, // re-kyc
		{StateFail, StateStarted}, // re-kyc
	}
	for _, c := range cases {
		if !CanTransition(c.from, c.to) {
			t.Errorf("expected legal %s -> %s", c.from, c.to)
		}
		if err := ValidateTransition(c.from, c.to); err != nil {
			t.Errorf("expected legal %s -> %s: %v", c.from, c.to, err)
		}
	}
}

func TestIllegalTransitions(t *testing.T) {
	type tc struct{ from, to State }
	cases := []tc{
		{StateStarted, StatePass},
		{StateStarted, StateLivenessPassed},
		{StateDocumentsUploaded, StateVendorDecision},
		{StateLivenessPassed, StatePass},
		{StateScreening, StatePass},
		{StatePass, StateFail},
		{StateFail, StatePass},
		{StateManualReview, StateScreening},
	}
	for _, c := range cases {
		if CanTransition(c.from, c.to) {
			t.Errorf("expected illegal %s -> %s to be disallowed", c.from, c.to)
		}
		if err := ValidateTransition(c.from, c.to); err == nil {
			t.Errorf("expected error for %s -> %s", c.from, c.to)
		}
	}
}

func TestReKYCOnlyFromTerminal(t *testing.T) {
	if _, err := ReKYC(StateStarted); err == nil {
		t.Fatal("expected re-kyc from started to fail")
	}
	if s, err := ReKYC(StatePass); err != nil || s != StateStarted {
		t.Fatalf("re-kyc from pass: s=%s err=%v", s, err)
	}
	if s, err := ReKYC(StateFail); err != nil || s != StateStarted {
		t.Fatalf("re-kyc from fail: s=%s err=%v", s, err)
	}
}

// ---- Repository tests ----

func TestRepoCreateDuplicateUser(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app1 := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app1); err != nil {
		t.Fatal(err)
	}
	app2 := &Application{ID: "a2", UserID: "u1", State: StateStarted}
	if err := repo.Create(app2); err == nil {
		t.Fatal("expected duplicate user error")
	}
}

func TestRepoCreateAfterTerminalAllowsReplacement(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app1 := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app1); err != nil {
		t.Fatal(err)
	}
	setState(repo, "a1", StateDocumentsUploaded)
	setState(repo, "a1", StateLivenessPassed)
	setState(repo, "a1", StateScreening)
	setState(repo, "a1", StateVendorDecision)
	setState(repo, "a1", StatePass)
	app2 := &Application{ID: "a2", UserID: "u1", State: StateStarted}
	if err := repo.Create(app2); err != nil {
		t.Fatalf("expected new app after terminal, got: %v", err)
	}
}

func TestRepoUpdateStateIllegalTransition(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateState("a1", app.Version, StatePass, "test", "x"); err == nil {
		t.Fatal("expected illegal transition error")
	}
}

func TestRepoUpdateStateVersionConflict(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	v := app.Version
	// first succeeds, increments version
	if _, err := repo.UpdateState("a1", v, StateDocumentsUploaded, "t", "x"); err != nil {
		t.Fatal(err)
	}
	// second with stale version must fail
	if _, err := repo.UpdateState("a1", v, StateLivenessPassed, "t", "x"); err == nil {
		t.Fatal("expected version conflict")
	}
}

func TestConcurrencyConflict(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	get := func() (int, State) {
		a, _ := repo.Get("a1")
		return a.Version, a.State
	}
	var wg sync.WaitGroup
	var ok, fail int
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := get()
			_, err := repo.UpdateState("a1", v, StateDocumentsUploaded, "race", "x")
			mu.Lock()
			if err == nil {
				ok++
			} else {
				fail++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("expected exactly 1 successful update, got %d", ok)
	}
	if fail != 49 {
		t.Fatalf("expected 49 conflicts, got %d", fail)
	}
}

func TestReopenNotTerminal(t *testing.T) {
	repo := NewApplicationRepository(nil)
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Reopen("a1", app.Version, "test"); err == nil {
		t.Fatal("expected reopen of non-terminal to fail")
	}
}

func TestReopenTerminal(t *testing.T) {
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
	if _, err := repo.Reopen("a1", a.Version, "test"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	a2, _ := repo.Get("a1")
	if a2.State != StateStarted {
		t.Fatalf("expected started, got %s", a2.State)
	}
	if !a2.DecidedAt.IsZero() {
		t.Fatal("decided_at should be reset")
	}
}

func TestListDueForReKYC(t *testing.T) {
	repo := NewApplicationRepository(nil)
	repo.now = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }
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
	// re_kyc_due_at = decided + 365d = 2026-01-01
	due := a.ReKYCDueAt
	now := due.Add(time.Hour)
	dueList := repo.ListDueForReKYC(now)
	if len(dueList) != 1 || dueList[0].ID != "a1" {
		t.Fatalf("expected a1 due, got %+v", dueList)
	}
	nowBefore := due.Add(-time.Hour)
	if len(repo.ListDueForReKYC(nowBefore)) != 0 {
		t.Fatal("expected not due yet")
	}
}

// ---- HTTP API tests ----

func TestHTTPCreateApplicationSuccess(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()

	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications", map[string]string{
		"user_id":   "u-1",
		"full_name": "Good Person",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out createAppResponse
	decodeBody(t, resp, &out)
	if out.ID == "" || out.UserID != "u-1" || out.State != StateStarted {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestHTTPCreateApplicationDuplicateRejected(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()

	id1 := createApp(t, srv, "u-dup")
	if id1 == "" {
		t.Fatal("expected first app")
	}
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications", map[string]string{
		"user_id": "u-dup",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	var env errorEnvelope
	decodeBody(t, resp, &env)
	if env.Error.Code != "duplicate_application" {
		t.Fatalf("unexpected code %s", env.Error.Code)
	}
}

func TestHTTPCreateApplicationMissingUserID(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications", map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPGetApplicationSuccess(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-get")
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out applicationView
	decodeBody(t, resp, &out)
	if out.ID != id {
		t.Fatalf("expected id %s got %s", id, out.ID)
	}
}

func TestHTTPGetApplicationNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	var env errorEnvelope
	decodeBody(t, resp, &env)
	if env.Error.Code != "application_not_found" {
		t.Fatalf("expected application_not_found, got %s", env.Error.Code)
	}
	if env.Error.RequestID == "" {
		t.Fatal("expected request_id in error envelope")
	}
}

func TestHTTPGetStatusSuccess(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-status")
	_ = id
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/status/u-status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out statusResponse
	decodeBody(t, resp, &out)
	if out.UserID != "u-status" || out.State != StateStarted {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestHTTPGetStatusNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/status/none", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPStatusReturnsDecisionTimestamp(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-dec")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	setState(s.Repo, id, StateScreening)
	setState(s.Repo, id, StateVendorDecision)
	setState(s.Repo, id, StatePass)
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/status/u-dec", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out statusResponse
	decodeBody(t, resp, &out)
	if out.DecidedAt == nil {
		t.Fatal("expected decided_at")
	}
	if out.ReKYCDueAt == nil {
		t.Fatal("expected re_kyc_due_at")
	}
}

// ---- Documents ----

func TestDocumentUploadSuccess(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-doc")
	uploadDoc(t, srv, id, "ID_FRONT")
	uploadDoc(t, srv, id, "SELFIE")
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/"+id+"/documents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Documents []Document `json:"documents"`
	}
	decodeBody(t, resp, &out)
	if len(out.Documents) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(out.Documents))
	}
}

func TestDocumentUploadBadType(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-doc-bad")
	resp := doJSON(t, srv.Client(), http.MethodPost,
		srv.URL+"/v1/kyc/applications/"+id+"/documents",
		map[string]string{"type": "passport", "content": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDocumentUploadTransitionsToDocumentsUploaded(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-doc-trans")
	uploadDoc(t, srv, id, "ID_FRONT")
	// not yet
	app, _ := s.Repo.Get(id)
	if app.State != StateStarted {
		t.Fatalf("expected started, got %s", app.State)
	}
	uploadDoc(t, srv, id, "SELFIE")
	app, _ = s.Repo.Get(id)
	if app.State != StateDocumentsUploaded {
		t.Fatalf("expected documents_uploaded, got %s", app.State)
	}
}

func TestDocumentRetention365Days(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-doc-ret")
	resp := doJSON(t, srv.Client(), http.MethodPost,
		srv.URL+"/v1/kyc/applications/"+id+"/documents",
		map[string]string{"type": "SELFIE", "content": "x"})
	decodeBody(t, resp, &Document{})
	var doc Document
	// re-decode via documents list
	listResp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/"+id+"/documents", nil)
	var out struct {
		Documents []Document `json:"documents"`
	}
	decodeBody(t, listResp, &out)
	if len(out.Documents) != 1 {
		t.Fatal("expected 1 doc")
	}
	doc = out.Documents[0]
	delta := doc.RetentionUntil.Sub(doc.UploadedAt)
	if delta != 365*24*time.Hour {
		t.Fatalf("expected 365d retention, got %v", delta)
	}
}

// ---- Liveness ----

func TestLivenessSuccessTransitions(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-live")
	uploadDoc(t, srv, id, "ID_FRONT")
	uploadDoc(t, srv, id, "SELFIE")
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/liveness", nil)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 201, got %d body %s", resp.StatusCode, b)
	}
	var sess LivenessSession
	decodeBody(t, resp, &sess)
	if sess.Status != "PASSED" {
		t.Fatalf("expected passed, got %s", sess.Status)
	}
	app, _ := s.Repo.Get(id)
	if app.State != StateLivenessPassed {
		t.Fatalf("expected liveness_passed, got %s", app.State)
	}
	// GET liveness
	gresp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/"+id+"/liveness", nil)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", gresp.StatusCode)
	}
	gresp.Body.Close()
}

func TestLivenessFromStartedAllowed(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-live2")
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/liveness", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from started, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---- Screening ----

func TestScreeningCleanTransitionsToScreening(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-scr")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening", map[string]string{
		"full_name": "Good Person",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Hits         []SanctionsHit `json:"hits"`
		ManualReview bool           `json:"manual_review"`
	}
	decodeBody(t, resp, &out)
	if len(out.Hits) != 0 {
		t.Fatalf("expected 0 hits, got %d", len(out.Hits))
	}
	app, _ := s.Repo.Get(id)
	if app.State != StateScreening {
		t.Fatalf("expected screening, got %s", app.State)
	}
}

func TestScreeningHitTransitionsToManualReview(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-scr-hit")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening", map[string]string{
		"full_name": "BAD ACTOR",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Hits         []SanctionsHit `json:"hits"`
		ManualReview bool           `json:"manual_review"`
	}
	decodeBody(t, resp, &out)
	if len(out.Hits) == 0 || !out.ManualReview {
		t.Fatalf("expected hits and manual review, got %+v", out)
	}
	app, _ := s.Repo.Get(id)
	if app.State != StateManualReview {
		t.Fatalf("expected manual_review, got %s", app.State)
	}
}

func TestScreeningDispositionPass(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-scr-disp")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	// trigger manual review via screening with bad name
	_, _ = doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening", map[string]string{
		"full_name": "EVIL DOER",
	}).Body.Read(make([]byte, 0))
	app, _ := s.Repo.Get(id)
	if app.State != StateManualReview {
		t.Fatalf("expected manual_review, got %s", app.State)
	}
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening/disposition", map[string]string{
		"disposition": "CLEAR",
		"reviewed_by": "analyst1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	app, _ = s.Repo.Get(id)
	if app.State != StatePass {
		t.Fatalf("expected pass, got %s", app.State)
	}
}

func TestScreeningDispositionFail(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-scr-disp2")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	_, _ = doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening", map[string]string{
		"full_name": "EVIL DOER",
	}).Body.Read(make([]byte, 0))
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening/disposition", map[string]string{
		"disposition": "BLOCK",
		"reviewed_by": "analyst1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	app, _ := s.Repo.Get(id)
	if app.State != StateFail {
		t.Fatalf("expected fail, got %s", app.State)
	}
}

func TestScreeningDispositionBadValue(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-scr-bad")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening/disposition", map[string]string{
		"disposition": "maybe",
		"reviewed_by": "analyst1",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---- Webhooks ----

func signWebhook(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookValidHMACAccepted(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()

	body := []byte(`{"event":"check.completed","application_id":"wh-1"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, ts, body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/stub", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", "v1="+sig)
	req.Header.Set("X-Webhook-Timestamp", ts)
	req.Header.Set("X-Webhook-Id", "evt-1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWebhookInvalidHMACRejected(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()

	body := []byte(`{"event":"x"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/stub", bytes.NewReader(body))
	req.Header.Set("X-Webhook-Signature", "v1=deadbeef")
	req.Header.Set("X-Webhook-Timestamp", ts)
	req.Header.Set("X-Webhook-Id", "evt-bad")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWebhookStaleTimestampRejected(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()

	body := []byte(`{"event":"x"}`)
	stale := fmt.Sprintf("%d", time.Now().Add(-1*time.Hour).Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, stale, body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/stub", bytes.NewReader(body))
	req.Header.Set("X-Webhook-Signature", "v1="+sig)
	req.Header.Set("X-Webhook-Timestamp", stale)
	req.Header.Set("X-Webhook-Id", "evt-stale")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWebhookDuplicateIdempotent(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()

	body := []byte(`{"event":"x","application_id":"wh-dup"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, ts, body)

	send := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/stub", bytes.NewReader(body))
		req.Header.Set("X-Webhook-Signature", "v1="+sig)
		req.Header.Set("X-Webhook-Timestamp", ts)
		req.Header.Set("X-Webhook-Id", "evt-dup")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := send()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first: expected 200, got %d", r1.StatusCode)
	}
	r1.Body.Close()
	r2 := send()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("second: expected 200, got %d", r2.StatusCode)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if !strings.Contains(string(b), "duplicate") {
		t.Fatalf("expected duplicate in body, got %s", b)
	}
}

// ---- Re-KYC ----

func TestReKYCTriggerReopensTerminal(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-rekyc")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	setState(s.Repo, id, StateScreening)
	setState(s.Repo, id, StateVendorDecision)
	setState(s.Repo, id, StatePass)
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/internal/v1/rekyc/trigger", map[string]string{
		"user_id": "u-rekyc",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	app, _ := s.Repo.Get(id)
	if app.State != StateStarted {
		t.Fatalf("expected started, got %s", app.State)
	}
}

func TestReKYCTriggerNonTerminal(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	createApp(t, srv, "u-rekyc-2")
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/internal/v1/rekyc/trigger", map[string]string{
		"user_id": "u-rekyc-2",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReKYCSchedulerTick(t *testing.T) {
	s := newTestServices()
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := s.Repo.Create(app); err != nil {
		t.Fatal(err)
	}
	setState(s.Repo, "a1", StateDocumentsUploaded)
	setState(s.Repo, "a1", StateLivenessPassed)
	setState(s.Repo, "a1", StateScreening)
	setState(s.Repo, "a1", StateVendorDecision)
	setState(s.Repo, "a1", StatePass)
	a, _ := s.Repo.Get("a1")
	due := a.ReKYCDueAt
	n := s.ReKYC.Tick(due.Add(time.Hour))
	if n != 1 {
		t.Fatalf("expected 1 reopen, got %d", n)
	}
	a2, _ := s.Repo.Get("a1")
	if a2.State != StateStarted {
		t.Fatalf("expected started, got %s", a2.State)
	}
}

// ---- Audit ----

func TestAuditEventsEmittedForTransitions(t *testing.T) {
	s := newTestServices()
	repo := s.Repo
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := repo.Create(app); err != nil {
		t.Fatal(err)
	}
	setState(repo, "a1", StateDocumentsUploaded)
	setState(repo, "a1", StateLivenessPassed)
	events := s.Audit.List()
	found := 0
	for _, e := range events {
		if e.Action == "state_transition" {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("expected >=2 transition events, got %d", found)
	}
}

func TestHTTPAuditEventsEndpoint(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	createApp(t, srv, "u-aud")
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/audit-events", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Events []AuditEvent `json:"events"`
	}
	decodeBody(t, resp, &out)
	if len(out.Events) == 0 {
		t.Fatal("expected audit events")
	}
}

// ---- Error envelope + correlation id ----

func TestErrorEnvelopeAndCorrelationID(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	cid := "test-correlation-id"
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/kyc/applications/none", nil)
	req.Header.Set("X-Correlation-Id", cid)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Correlation-Id") != cid {
		t.Fatalf("expected correlation id echoed, got %q", resp.Header.Get("X-Correlation-Id"))
	}
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code == "" || env.Error.Message == "" || env.Error.RequestID != cid {
		t.Fatalf("bad error envelope: %+v", env)
	}
}

func TestCorrelationIDGeneratedWhenAbsent(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/kyc/applications/none", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Correlation-Id") == "" {
		t.Fatal("expected generated correlation id")
	}
}

// ---- /readyz ----

func TestReadyz(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/readyz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "ready") {
		t.Fatalf("expected ready in body, got %s", b)
	}
}

// ---- Vendor stub ----

func TestStubVendorClient(t *testing.T) {
	c := &StubVendorClient{}
	ctx := context.Background()
	id, err := c.CreateApplicant(ctx, VendorApplicant{ApplicationID: "a1", UserID: "u1", FullName: "X"})
	if err != nil || id == "" {
		t.Fatalf("create applicant: %v %s", err, id)
	}
	docID, err := c.UploadDocument(ctx, id, VendorDocument{Type: "ID_FRONT", Content: []byte("x")})
	if err != nil || docID == "" {
		t.Fatalf("upload doc: %v %s", err, docID)
	}
	sess, err := c.StartLiveness(ctx, id)
	if err != nil || sess == "" {
		t.Fatalf("start liveness: %v %s", err, sess)
	}
	rep, err := c.GetReport(ctx, id)
	if err != nil || !rep.LivenessPassed {
		t.Fatalf("get report: %v %+v", err, rep)
	}
	evt, err := c.ParseWebhook(ctx, "stub", []byte("x"))
	if err != nil || evt.EventID == "" {
		t.Fatalf("parse webhook: %v %+v", err, evt)
	}
}

func TestNewVendorClientUnknown(t *testing.T) {
	t.Setenv("VENDOR_PROVIDER", "nonexistent")
	_, err := NewVendorClient()
	if err == nil {
		t.Fatal("expected error for unknown vendor")
	}
}

func TestNewVendorClientDefaultStub(t *testing.T) {
	t.Setenv("VENDOR_PROVIDER", "")
	c, err := NewVendorClient()
	if err != nil || c == nil {
		t.Fatalf("expected default stub, got %v", err)
	}
	if c.Name() != "stub" {
		t.Fatalf("expected stub, got %s", c.Name())
	}
}

// ---- Screening store & client ----

func TestInMemoryScreeningClientMatch(t *testing.T) {
	c := NewInMemoryScreeningClient()
	hits, err := c.Screen(context.Background(), "John BAD ACTOR Smith")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].MatchedName != "BAD ACTOR" {
		t.Fatalf("expected 1 BAD ACTOR hit, got %+v", hits)
	}
	hits, _ = c.Screen(context.Background(), "Jane Doe")
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits for clean name, got %+v", hits)
	}
}

func TestScreeningStoreAddListDisposition(t *testing.T) {
	st := NewScreeningStore()
	st.Add("a1", SanctionsHit{ID: "h1", ApplicationID: "a1"})
	st.Add("a1", SanctionsHit{ID: "h2", ApplicationID: "a1"})
	if len(st.List("a1")) != 2 {
		t.Fatal("expected 2 hits")
	}
	st.SetDisposition("a1", "analyst", "BLOCK")
	for _, h := range st.List("a1") {
		if h.Disposition != "BLOCK" || h.ReviewedBy != "analyst" {
			t.Fatalf("expected disposition set, got %+v", h)
		}
	}
}

// ---- Webhook verification unit ----

func TestVerifyWebhookStripeStyle(t *testing.T) {
	body := []byte(`{"x":1}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, ts, body)
	header := fmt.Sprintf("t=%s,v1=%s", ts, sig)
	if err := VerifyWebhook(body, header, "", secret, 300*time.Second); err != nil {
		t.Fatalf("stripe-style verify: %v", err)
	}
}

func TestVerifyWebhookMissingSig(t *testing.T) {
	body := []byte(`x`)
	if err := VerifyWebhook(body, "", "1", "s", 300*time.Second); err != errMissingSig {
		t.Fatalf("expected errMissingSig, got %v", err)
	}
}

func TestVerifyWebhookMissingTs(t *testing.T) {
	body := []byte(`x`)
	if err := VerifyWebhook(body, "v1=abc", "", "s", 300*time.Second); err != errMissingTs {
		t.Fatalf("expected errMissingTs, got %v", err)
	}
}

// ---- UUID ----

func TestNewUUIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newUUID()
		if seen[id] {
			t.Fatal("duplicate uuid")
		}
		seen[id] = true
	}
}

// ---- Document upload via multipart ----

func TestDocumentUploadMultipart(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-mp")
	body := &bytes.Buffer{}
	body.WriteString("--boundary\r\n")
	body.WriteString(`Content-Disposition: form-data; name="type"` + "\r\n\r\n")
	body.WriteString("ID_FRONT\r\n")
	body.WriteString("--boundary\r\n")
	body.WriteString(`Content-Disposition: form-data; name="content"` + "\r\n\r\n")
	body.WriteString("raw-bytes\r\n")
	body.WriteString("--boundary--\r\n")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/documents", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 201, got %d body %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	// Only ID_FRONT uploaded via multipart; required set (ID_FRONT+SELFIE) not satisfied.
	if s.Docs.HasRequiredDocs(id) {
		t.Fatal("expected required docs NOT satisfied with only ID_FRONT")
	}
}

// ---- Screening disposition not in review ----

func TestScreeningDispositionNotInReview(t *testing.T) {
	s := newTestServices()
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := s.Repo.Create(app); err != nil {
		t.Fatal(err)
	}
	err := s.Screen.Disposition(context.Background(), "a1", "CLEAR", "analyst")
	if err == nil {
		t.Fatal("expected error: not in review")
	}
}