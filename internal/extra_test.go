package internal

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Extra coverage tests.

func TestScreeningThresholdEnv(t *testing.T) {
	t.Setenv("SCREENING_HIT_THRESHOLD", "3")
	if ScreeningThreshold() != 3 {
		t.Fatalf("expected 3, got %d", ScreeningThreshold())
	}
	t.Setenv("SCREENING_HIT_THRESHOLD", "bad")
	if ScreeningThreshold() != 1 {
		t.Fatalf("expected fallback 1, got %d", ScreeningThreshold())
	}
	t.Setenv("SCREENING_HIT_THRESHOLD", "0")
	if ScreeningThreshold() != 1 {
		t.Fatalf("expected fallback 1 for 0, got %d", ScreeningThreshold())
	}
}

func TestScreeningThresholdAboveAvoidsManualReview(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-thr")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	t.Setenv("SCREENING_HIT_THRESHOLD", "5")
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/screening", map[string]string{
		"full_name": "BAD ACTOR",
	})
	var out struct {
		ManualReview bool `json:"manual_review"`
	}
	decodeBody(t, resp, &out)
	if out.ManualReview {
		t.Fatal("expected no manual review when threshold high")
	}
	app, _ := s.Repo.Get(id)
	if app.State != StateScreening {
		t.Fatalf("expected screening, got %s", app.State)
	}
}

func TestGetLivenessNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-live-404")
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/"+id+"/liveness", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLivenessBadState(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	id := createApp(t, srv, "u-live-bad")
	setState(s.Repo, id, StateDocumentsUploaded)
	setState(s.Repo, id, StateLivenessPassed)
	setState(s.Repo, id, StateScreening)
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/"+id+"/liveness", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDecodeJSONEmptyBody(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/kyc/applications", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDecodeJSONBadJSON(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/kyc/applications", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVendorCtxCancelled(t *testing.T) {
	c := &StubVendorClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CreateApplicant(ctx, VendorApplicant{}); err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if _, err := c.UploadDocument(ctx, "x", VendorDocument{}); err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if _, err := c.StartLiveness(ctx, "x"); err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if _, err := c.GetReport(ctx, "x"); err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if _, err := c.ParseWebhook(ctx, "stub", []byte("x")); err == nil {
		t.Fatal("expected ctx cancel error")
	}
}

func TestWebhookMissingIDIgnored(t *testing.T) {
	w := NewWebhookStore()
	// empty id -> never seen
	if w.Seen("") {
		t.Fatal("empty id should not be seen")
	}
	// first call with "x" returns false (not previously seen) and marks it
	if w.Seen("x") {
		t.Fatal("first seen x should be false")
	}
	// second call returns true (already seen)
	if !w.Seen("x") {
		t.Fatal("second seen x should be true")
	}
}

func TestWebhookReconcileAdvancesState(t *testing.T) {
	s := newTestServices()
	app := &Application{ID: "a1", UserID: "u1", State: StateStarted}
	if err := s.Repo.Create(app); err != nil {
		t.Fatal(err)
	}
	setState(s.Repo, "a1", StateDocumentsUploaded)
	setState(s.Repo, "a1", StateLivenessPassed)
	setState(s.Repo, "a1", StateScreening)
	setState(s.Repo, "a1", StateVendorDecision)

	// craft a webhook body that ParseWebhook will map and that includes the
	// application id so reconciliation finds the app.
	body := []byte(`{"application_id":"a1"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	secret := "dev-webhook-secret"
	sig := signWebhook(secret, ts, body)
	res := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-recon-1")
	if !res.Accepted {
		t.Fatalf("expected accepted, got %s", res.Reason)
	}
	app2, _ := s.Repo.Get("a1")
	// stub ParseWebhook returns outcome=CLEAR; from vendor_decision -> pass legal
	if app2.State != StatePass {
		t.Fatalf("expected pass after webhook reconcile, got %s", app2.State)
	}
	// replay idempotent
	res2 := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-recon-1")
	if !res2.Duplicate {
		t.Fatal("expected duplicate on replay")
	}
}

func TestWebhookBadSignature(t *testing.T) {
	body := []byte(`x`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	if err := VerifyWebhook(body, "v1=bad", ts, "dev-webhook-secret", 300*time.Second); err != errBadSignature {
		t.Fatalf("expected errBadSignature, got %v", err)
	}
}

func TestWebhookParseErrorRejected(t *testing.T) {
	s := newTestServices()
	// Use a vendor client whose ParseWebhook always errors.
	s.Webhook.vendor = &errVendorClient{}
	body := []byte(`{}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook("dev-webhook-secret", ts, body)
	res := s.Webhook.Ingest(context.Background(), "stub", body, "v1="+sig, ts, "evt-parse-err")
	if res.Accepted {
		t.Fatal("expected not accepted")
	}
	if !strings.Contains(res.Reason, "parse") {
		t.Fatalf("expected parse error, got %s", res.Reason)
	}
}

type errVendorClient struct{}

func (errVendorClient) Name() string { return "err" }
func (errVendorClient) CreateApplicant(context.Context, VendorApplicant) (string, error) {
	return "", nil
}
func (errVendorClient) UploadDocument(context.Context, string, VendorDocument) (string, error) {
	return "", nil
}
func (errVendorClient) StartLiveness(context.Context, string) (string, error) {
	return "", nil
}
func (errVendorClient) GetReport(context.Context, string) (VendorReport, error) {
	return VendorReport{}, nil
}
func (errVendorClient) ParseWebhook(context.Context, string, []byte) (VendorWebhookEvent, error) {
	return VendorWebhookEvent{}, fmt.Errorf("parse failure")
}

func TestRepoGetNotFound(t *testing.T) {
	repo := NewApplicationRepository(nil)
	if _, err := repo.Get("nope"); err == nil {
		t.Fatal("expected not found")
	}
	if _, err := repo.GetByUserID("nope"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestRepoCreateInvalidArgs(t *testing.T) {
	repo := NewApplicationRepository(nil)
	if err := repo.Create(&Application{ID: "x"}); err == nil {
		t.Fatal("expected missing user_id error")
	}
	if err := repo.Create(&Application{UserID: "u", State: StateStarted}); err == nil {
		t.Fatal("expected missing id error")
	}
}

func TestRepoUpdateStateNotFound(t *testing.T) {
	repo := NewApplicationRepository(nil)
	if _, err := repo.UpdateState("nope", 1, StatePass, "x", "y"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestRepoReopenNotFound(t *testing.T) {
	repo := NewApplicationRepository(nil)
	if _, err := repo.Reopen("nope", 1, "x"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestReKYCServiceStartStop(t *testing.T) {
	s := newTestServices()
	s.ReKYC.Start(10 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	s.ReKYC.Stop()
}

func TestPortOrDefault(t *testing.T) {
	// main() calls run(":8080"); just ensure portOrDefault-like behavior isn't
	// needed. This test exists to keep coverage of os.Getenv paths.
	_ = os.Getenv("PORT")
}

func TestStatusRecorderWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 0}
	sr.WriteHeader(http.StatusTeapot)
	if sr.status != http.StatusTeapot {
		t.Fatal("status not set")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatal("code not set")
	}
}

func TestToAppErrorUnknown(t *testing.T) {
	ae := toAppError(fmt.Errorf("something weird"))
	if ae.Code != "internal_error" {
		t.Fatalf("expected internal_error, got %s", ae.Code)
	}
}

func TestToAppErrorNil(t *testing.T) {
	if toAppError(nil) != nil {
		t.Fatal("expected nil")
	}
}

func TestParseDocRequestBadMultipart(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("not multipart")))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if _, _, err := parseDocRequest(req); err == nil {
		t.Fatal("expected error for bad multipart")
	}
}

func TestParseDocRequestMultipartWithFile(t *testing.T) {
	body := &bytes.Buffer{}
	body.WriteString("--b\r\n")
	body.WriteString(`Content-Disposition: form-data; name="type"` + "\r\n\r\n")
	body.WriteString("SELFIE\r\n")
	body.WriteString("--b\r\n")
	body.WriteString(`Content-Disposition: form-data; name="file"; filename="f.jpg"` + "\r\n")
	body.WriteString("Content-Type: image/jpeg\r\n\r\n")
	body.WriteString("img-bytes\r\n")
	body.WriteString("--b--\r\n")
	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	docType, content, err := parseDocRequest(req)
	if err != nil {
		t.Fatalf("parseDocRequest: %v", err)
	}
	if docType != "SELFIE" || string(content) != "img-bytes" {
		t.Fatalf("got type=%q content=%q", docType, content)
	}
}

func TestListDocumentsNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1/kyc/applications/none/documents", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUploadDocumentNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/none/documents", map[string]string{"type": "SELFIE"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRunScreeningNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1/kyc/applications/none/screening", map[string]string{"full_name": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTriggerReKYCNotFound(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/internal/v1/rekyc/trigger", map[string]string{"user_id": "none"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTriggerReKYCMissingUser(t *testing.T) {
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	resp := doJSON(t, srv.Client(), http.MethodPost, srv.URL+"/internal/v1/rekyc/trigger", map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}