package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// OnfidoVendorClient implements VendorClient against the Onfido API
// (https://api.{region}.onfido.com/v3.6). It uses net/http only — no SDK.
// KYC_VENDOR_URL is the API base (e.g. https://api.eu.onfido.com) and
// ONFIDO_API_TOKEN (or KYC_VENDOR_API_KEY) is the bearer token.
type OnfidoVendorClient struct {
	base   string
	token  string
	http   *http.Client
	tracer trace.Tracer
}

// NewOnfidoVendorClient builds an OnfidoVendorClient from env. baseURL falls
// back to KYC_VENDOR_URL; token falls back from ONFIDO_API_TOKEN to
// KYC_VENDOR_API_KEY.
func NewOnfidoVendorClient(baseURL, token string, timeout time.Duration) *OnfidoVendorClient {
	if baseURL == "" {
		baseURL = os.Getenv("KYC_VENDOR_URL")
	}
	if token == "" {
		token = os.Getenv("ONFIDO_API_TOKEN")
	}
	if token == "" {
		token = os.Getenv("KYC_VENDOR_API_KEY")
	}
	if timeout <= 0 {
		timeout = envDurationDefault("VENDOR_CALL_TIMEOUT", 30*time.Second)
	}
	return &OnfidoVendorClient{
		base:   strings.TrimRight(baseURL, "/"),
		token:  token,
		http:   &http.Client{Timeout: timeout},
		tracer: otel.Tracer("vendor.onfido"),
	}
}

// Name returns "onfido".
func (c *OnfidoVendorClient) Name() string { return "onfido" }

// onfidoApplicantResponse is the parsed response of POST /applicants.
type onfidoApplicantResponse struct {
	ID string `json:"id"`
}

// CreateApplicant creates an Onfido applicant and returns its id.
func (c *OnfidoVendorClient) CreateApplicant(ctx context.Context, app VendorApplicant) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	first, last := splitFullName(app.FullName)
	payload := map[string]any{
		"first_name": first,
		"last_name":  last,
		"email":      "",
	}
	if email := os.Getenv("APPLICANT_EMAIL"); email != "" {
		payload["email"] = email
	}
	body, err := c.doJSON(ctx, http.MethodPost, "/v3.6/applicants", payload)
	if err != nil {
		return "", err
	}
	defer body.Close()
	var resp onfidoApplicantResponse
	if err := decodeJSONReader(body, &resp); err != nil {
		return "", fmt.Errorf("onfido create applicant: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("onfido create applicant: empty id")
	}
	return resp.ID, nil
}

// UploadDocument uploads a document multipart to Onfido and returns the
// document id.
func (c *OnfidoVendorClient) UploadDocument(ctx context.Context, applicantID string, doc VendorDocument) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("type", onfidoDocType(doc.Type)); err != nil {
		return "", err
	}
	fw, err := mw.CreateFormFile("file", "upload")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(doc.Content); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	body, err := c.doMultipart(ctx, http.MethodPost, "/v3.6/applicants/"+applicantID+"/documents", &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer body.Close()
	var resp onfidoApplicantResponse
	if err := decodeJSONReader(body, &resp); err != nil {
		return "", fmt.Errorf("onfido upload document: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("onfido upload document: empty id")
	}
	return resp.ID, nil
}

// onfidoLivenessResponse is the parsed response of POST /liveness_checks.
type onfidoLivenessResponse struct {
	ID     string `json:"id"`
	Result string `json:"result"`
}

// StartLiveness starts a liveness check for the applicant and returns the
// liveness session id.
func (c *OnfidoVendorClient) StartLiveness(ctx context.Context, applicantID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	payload := map[string]any{"applicant_id": applicantID}
	body, err := c.doJSON(ctx, http.MethodPost, "/v3.6/applicants/"+applicantID+"/liveness_checks", payload)
	if err != nil {
		return "", err
	}
	defer body.Close()
	var resp onfidoLivenessResponse
	if err := decodeJSONReader(body, &resp); err != nil {
		return "", fmt.Errorf("onfido start liveness: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("onfido start liveness: empty id")
	}
	return resp.ID, nil
}

// onfidoCheck is a single check returned by GET /checks.
type onfidoCheck struct {
	ID        string `json:"id"`
	Result    string `json:"result"`
	Report    string `json:"report_ids"`
	SubResult string `json:"sub_result"`
}

// onfidoChecksResponse is the parsed response of GET /checks.
type onfidoChecksResponse struct {
	Data []onfidoCheck `json:"data"`
}

// GetReport fetches the latest check for the applicant and maps the result to
// CLEAR / CONSIDER / FAIL.
func (c *OnfidoVendorClient) GetReport(ctx context.Context, applicantID string) (VendorReport, error) {
	if err := ctx.Err(); err != nil {
		return VendorReport{}, err
	}
	body, err := c.doJSON(ctx, http.MethodGet, "/v3.6/applicants/"+applicantID+"/checks", nil)
	if err != nil {
		return VendorReport{}, err
	}
	defer body.Close()
	var resp onfidoChecksResponse
	if err := decodeJSONReader(body, &resp); err != nil {
		return VendorReport{}, fmt.Errorf("onfido get report: %w", err)
	}
	outcome, liveness, reason := "CLEAR", true, "no checks"
	if n := len(resp.Data); n > 0 {
		chk := resp.Data[n-1]
		outcome, liveness, reason = mapOnfidoResult(chk.Result, chk.SubResult)
	}
	return VendorReport{
		ApplicationID:  applicantID,
		Outcome:        outcome,
		LivenessPassed: liveness,
		SanctionsHits:  0,
		Reason:         reason,
	}, nil
}

// onfidoWebhookPayload is the parsed webhook body. Onfido wraps the event in
// a top-level object with a `payload` field.
type onfidoWebhookPayload struct {
	Payload struct {
		Action      string `json:"action"`
		Obj         string `json:"object"`
		Resource    string `json:"resource"`
		StatusCode  string `json:"status"`
		CompletedAt string `json:"completed_at"`
		ApplicantID string `json:"applicant_id"`
	} `json:"payload"`
}

// ParseWebhook parses an Onfido webhook payload (JSON).
func (c *OnfidoVendorClient) ParseWebhook(ctx context.Context, vendor string, raw []byte) (VendorWebhookEvent, error) {
	if err := ctx.Err(); err != nil {
		return VendorWebhookEvent{}, err
	}
	var p onfidoWebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return VendorWebhookEvent{}, fmt.Errorf("onfido parse webhook: %w", err)
	}
	outcome, liveness, _ := mapOnfidoResult(p.Payload.StatusCode, "")
	evt := VendorWebhookEvent{
		Vendor:         vendor,
		ApplicationID:  p.Payload.ApplicantID,
		Outcome:        outcome,
		LivenessPassed: liveness,
		Reason:         p.Payload.Action + ":" + p.Payload.Resource,
	}
	evt.EventID = p.Payload.Resource
	if evt.EventID == "" {
		evt.EventID = "onfido-" + p.Payload.Action
	}
	return evt, nil
}

// doJSON performs a JSON HTTP request with retry + tracing and returns the
// response body (caller closes).
func (c *OnfidoVendorClient) doJSON(ctx context.Context, method, path string, body any) (io.ReadCloser, error) {
	ctx, span := c.tracer.Start(ctx, "onfido."+method, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		verr := fmt.Errorf("onfido %s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
		span.RecordError(verr)
		span.SetStatus(codes.Error, verr.Error())
		return nil, verr
	}
	return resp.Body, nil
}

// doMultipart performs a multipart HTTP request and returns the response body
// (caller closes).
func (c *OnfidoVendorClient) doMultipart(ctx context.Context, method, path string, body *bytes.Buffer, contentType string) (io.ReadCloser, error) {
	ctx, span := c.tracer.Start(ctx, "onfido."+method, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		verr := fmt.Errorf("onfido %s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
		span.RecordError(verr)
		span.SetStatus(codes.Error, verr.Error())
		return nil, verr
	}
	return resp.Body, nil
}

// decodeJSONReader reads + JSON-decodes from r.
func decodeJSONReader(r io.Reader, dst any) error {
	return json.NewDecoder(r).Decode(dst)
}

// splitFullName splits a full name into first + last; the last token is the
// last name and the remainder is the first name.
func splitFullName(full string) (string, string) {
	parts := strings.Fields(strings.TrimSpace(full))
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return strings.Join(parts[:len(parts)-1], " "), parts[len(parts)-1]
}

// onfidoDocType maps internal document types to Onfido document types.
func onfidoDocType(t string) string {
	switch t {
	case "ID_FRONT":
		return "driving_licence"
	case "ID_BACK":
		return "driving_licence"
	case "SELFIE":
		return "selfie"
	case "POA":
		return "utility_bill"
	default:
		return strings.ToLower(t)
	}
}

// mapOnfidoResult maps an Onfido check result + sub_result to the internal
// CLEAR/CONSIDER/FAIL outcome and a liveness pass flag.
func mapOnfidoResult(result, subResult string) (outcome string, liveness bool, reason string) {
	switch strings.ToUpper(result) {
	case "CLEAR":
		return "CLEAR", true, "clear"
	case "CONSIDER", "UNIDENTIFIED", "RETRY":
		return "CONSIDER", true, "consider:" + subResult
	case "REJECTED", "FAIL":
		return "FAIL", false, "rejected:" + subResult
	default:
		if result == "" {
			return "CLEAR", true, "no-result"
		}
		return "CONSIDER", true, "unknown:" + result
	}
}
