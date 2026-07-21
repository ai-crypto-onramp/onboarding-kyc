package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnfidoCreateApplicant(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "apl_123"})
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	id, err := c.CreateApplicant(context.Background(), VendorApplicant{ApplicationID: "a1", FullName: "John Smith"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "apl_123" {
		t.Fatalf("id: %s", id)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth: %s", gotAuth)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("ct: %s", gotCT)
	}
	if gotBody["first_name"] != "John" || gotBody["last_name"] != "Smith" {
		t.Fatalf("body: %+v", gotBody)
	}
}

func TestOnfidoCreateApplicantError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	_, err := c.CreateApplicant(context.Background(), VendorApplicant{FullName: "John"})
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("err: %v", err)
	}
}

func TestOnfidoUploadDocument(t *testing.T) {
	var gotCT string
	var gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		if !strings.HasPrefix(r.URL.Path, "/v3.6/applicants/") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		gotType = r.FormValue("type")
		_, _ = w.Write([]byte(`{"id":"doc_456"}`))
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	id, err := c.UploadDocument(context.Background(), "apl_123", VendorDocument{Type: "SELFIE", Content: []byte("xyz")})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "doc_456" {
		t.Fatalf("id: %s", id)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Fatalf("ct: %s", gotCT)
	}
	if gotType != "selfie" {
		t.Fatalf("type: %s", gotType)
	}
}

func TestOnfidoStartLiveness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "live_789", "result": "CLEAR"})
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	id, err := c.StartLiveness(context.Background(), "apl_123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "live_789" {
		t.Fatalf("id: %s", id)
	}
}

func TestOnfidoGetReportClear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "chk_1", "result": "CLEAR"},
			},
		})
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	rep, err := c.GetReport(context.Background(), "apl_123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Outcome != "CLEAR" || !rep.LivenessPassed {
		t.Fatalf("report: %+v", rep)
	}
}

func TestOnfidoGetReportConsider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "chk_1", "result": "CONSIDER", "sub_result": "IDENTITY"},
			},
		})
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	rep, err := c.GetReport(context.Background(), "apl_123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Outcome != "CONSIDER" {
		t.Fatalf("expected CONSIDER, got %s", rep.Outcome)
	}
}

func TestOnfidoGetReportRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "chk_1", "result": "REJECTED"},
			},
		})
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	rep, err := c.GetReport(context.Background(), "apl_123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Outcome != "FAIL" || rep.LivenessPassed {
		t.Fatalf("report: %+v", rep)
	}
}

func TestOnfidoGetReportEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	c := NewOnfidoVendorClient(srv.URL, "tok", 0)
	rep, err := c.GetReport(context.Background(), "apl_123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Outcome != "CLEAR" {
		t.Fatalf("expected CLEAR, got %s", rep.Outcome)
	}
}

func TestOnfidoParseWebhook(t *testing.T) {
	c := NewOnfidoVendorClient("http://example.com", "tok", 0)
	raw := []byte(`{"payload":{"action":"check.completed","object":"check","resource":"chk_abc","status":"CLEAR","applicant_id":"apl_123"}}`)
	evt, err := c.ParseWebhook(context.Background(), "onfido", raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if evt.ApplicationID != "apl_123" || evt.Outcome != "CLEAR" || evt.EventID != "chk_abc" {
		t.Fatalf("event: %+v", evt)
	}
}

func TestOnfidoParseWebhookBadJSON(t *testing.T) {
	c := NewOnfidoVendorClient("http://example.com", "tok", 0)
	_, err := c.ParseWebhook(context.Background(), "onfido", []byte(`not-json`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOnfidoName(t *testing.T) {
	c := NewOnfidoVendorClient("http://example.com", "tok", 0)
	if c.Name() != "onfido" {
		t.Fatalf("name: %s", c.Name())
	}
}

func TestOnfidoContextCancelled(t *testing.T) {
	c := NewOnfidoVendorClient("http://example.com", "tok", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CreateApplicant(ctx, VendorApplicant{FullName: "John Smith"})
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestSplitFullName(t *testing.T) {
	first, last := splitFullName("John Jacob Smith")
	if first != "John Jacob" || last != "Smith" {
		t.Fatalf("split: %q / %q", first, last)
	}
	first, last = splitFullName("Solo")
	if first != "Solo" || last != "" {
		t.Fatalf("split solo: %q / %q", first, last)
	}
	first, last = splitFullName("")
	if first != "" || last != "" {
		t.Fatalf("split empty: %q / %q", first, last)
	}
}

func TestOnfidoDocTypeMapping(t *testing.T) {
	if onfidoDocType("ID_FRONT") != "driving_licence" {
		t.Fatal("ID_FRONT mapping")
	}
	if onfidoDocType("SELFIE") != "selfie" {
		t.Fatal("SELFIE mapping")
	}
	if onfidoDocType("POA") != "utility_bill" {
		t.Fatal("POA mapping")
	}
}

func TestMultipartWriterRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("type", "selfie")
	fw, _ := mw.CreateFormFile("file", "upload")
	_, _ = fw.Write([]byte("content"))
	_ = mw.Close()
	if !strings.HasPrefix(buf.String(), "--") {
		t.Fatal("multipart boundary missing")
	}
}

func TestNewVendorClientOnfido(t *testing.T) {
	t.Setenv("VENDOR_PROVIDER", "onfido")
	t.Setenv("KYC_VENDOR_URL", "http://example.com")
	t.Setenv("ONFIDO_API_TOKEN", "tok")
	c, err := NewVendorClientWithMode(false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if c.Name() != "onfido" {
		t.Fatalf("name: %s", c.Name())
	}
}

func TestNewVendorClientOnfidoMissingURLInProd(t *testing.T) {
	t.Setenv("VENDOR_PROVIDER", "onfido")
	t.Setenv("KYC_VENDOR_URL", "")
	t.Setenv("ONFIDO_API_TOKEN", "")
	_, err := NewVendorClientWithMode(false)
	if err == nil {
		t.Fatal("expected error for missing onfido config in prod")
	}
}

func TestNewVendorClientOnfidoFallsBackToStubInDev(t *testing.T) {
	t.Setenv("VENDOR_PROVIDER", "onfido")
	t.Setenv("KYC_VENDOR_URL", "")
	t.Setenv("ONFIDO_API_TOKEN", "")
	c, err := NewVendorClientWithMode(true)
	if err != nil || c.Name() != "stub" {
		t.Fatalf("expected stub fallback in dev, got %v / %s", err, c.Name())
	}
}

func TestNewVendorClientOnfidoReusesKYCVendorAPIKey(t *testing.T) {
	t.Setenv("VENDOR_PROVIDER", "onfido")
	t.Setenv("KYC_VENDOR_URL", "http://example.com")
	t.Setenv("ONFIDO_API_TOKEN", "")
	t.Setenv("KYC_VENDOR_API_KEY", "fallback-key")
	c, err := NewVendorClientWithMode(false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	oc, ok := c.(*OnfidoVendorClient)
	if !ok {
		t.Fatalf("expected *OnfidoVendorClient, got %T", c)
	}
	if oc.token != "fallback-key" {
		t.Fatalf("token: %s", oc.token)
	}
}
