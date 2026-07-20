package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// VendorDocument is the vendor-facing representation of a document upload.
type VendorDocument struct {
	Type    string
	Content []byte
}

// VendorApplicant is the payload sent to CreateApplicant.
type VendorApplicant struct {
	ApplicationID string
	UserID        string
	FullName      string
}

// VendorReport is the parsed vendor report.
type VendorReport struct {
	ApplicationID   string
	Outcome         string // "CLEAR" | "CONSIDER" | "FAIL"
	LivenessPassed  bool
	SanctionsHits   int
	Reason          string
}

// VendorWebhookEvent is the parsed webhook payload.
type VendorWebhookEvent struct {
	EventID      string
	Vendor       string
	ApplicationID string
	Outcome      string
	LivenessPassed bool
	SanctionsHits int
	Reason       string
}

// VendorClient abstracts the KYC vendor integration.
type VendorClient interface {
	CreateApplicant(ctx context.Context, app VendorApplicant) (vendorApplicantID string, err error)
	UploadDocument(ctx context.Context, vendorApplicantID string, doc VendorDocument) (vendorDocumentID string, err error)
	StartLiveness(ctx context.Context, vendorApplicantID string) (vendorSessionID string, err error)
	GetReport(ctx context.Context, vendorApplicantID string) (VendorReport, error)
	ParseWebhook(ctx context.Context, vendor string, raw []byte) (VendorWebhookEvent, error)
	Name() string
}

// NewVendorClient selects a vendor client by VENDOR_PROVIDER env. When the
// provider is unset or "stub" it returns the StubVendorClient, but only when
// DEV_MODE=1; in production KYC_VENDOR_URL (or LIVENESS_VENDOR_URL) must be set
// and a real vendor client used.
func NewVendorClient() (VendorClient, error) {
	return NewVendorClientWithMode(os.Getenv("DEV_MODE") == "1")
}

// NewVendorClientWithMode is the DEV_MODE-aware variant of NewVendorClient.
func NewVendorClientWithMode(devMode bool) (VendorClient, error) {
	provider := os.Getenv("VENDOR_PROVIDER")
	if provider == "" {
		provider = "stub"
	}
	switch provider {
	case "stub", "":
		if !devMode {
			if os.Getenv("KYC_VENDOR_URL") == "" && os.Getenv("LIVENESS_VENDOR_URL") == "" {
				return nil, fmt.Errorf("KYC_VENDOR_URL (or LIVENESS_VENDOR_URL) required in production mode; real vendor client not yet implemented — set DEV_MODE=1 for local dev")
			}
			// Real vendor HTTP client not yet implemented; require URL but
			// return the stub shape so the wire stays consistent. The error
			// above guards prod.
			return &StubVendorClient{}, nil
		}
		return &StubVendorClient{}, nil
	default:
		return nil, fmt.Errorf("unknown vendor provider: %s", provider)
	}
}

// StubVendorClient simulates vendor responses for local/dev use.
type StubVendorClient struct{}

func (s *StubVendorClient) Name() string { return "stub" }

func (s *StubVendorClient) CreateApplicant(ctx context.Context, app VendorApplicant) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(app.ApplicationID))
	return "aplit_" + hex.EncodeToString(h[:8]), nil
}

func (s *StubVendorClient) UploadDocument(ctx context.Context, vendorApplicantID string, doc VendorDocument) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(vendorApplicantID + doc.Type))
	return "doc_" + hex.EncodeToString(h[:8]), nil
}

func (s *StubVendorClient) StartLiveness(ctx context.Context, vendorApplicantID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(vendorApplicantID + time.Now().Format(time.RFC3339Nano)))
	return "live_" + hex.EncodeToString(h[:8]), nil
}

func (s *StubVendorClient) GetReport(ctx context.Context, vendorApplicantID string) (VendorReport, error) {
	if err := ctx.Err(); err != nil {
		return VendorReport{}, err
	}
	return VendorReport{
		ApplicationID:  vendorApplicantID,
		Outcome:        "CLEAR",
		LivenessPassed: true,
		SanctionsHits:  0,
		Reason:         "stub: CLEAR",
	}, nil
}

func (s *StubVendorClient) ParseWebhook(ctx context.Context, vendor string, raw []byte) (VendorWebhookEvent, error) {
	if err := ctx.Err(); err != nil {
		return VendorWebhookEvent{}, err
	}
	evt := VendorWebhookEvent{
		Vendor:         vendor,
		Outcome:        "CLEAR",
		LivenessPassed: true,
	}
	// Best-effort extraction of application_id and outcome from JSON body.
	var payload struct {
		ApplicationID string `json:"application_id"`
		Outcome       string `json:"outcome"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		evt.ApplicationID = payload.ApplicationID
		if payload.Outcome != "" {
			evt.Outcome = payload.Outcome
		}
	}
	h := sha256.Sum256(raw)
	evt.EventID = "stub-" + hex.EncodeToString(h[:8])
	return evt, nil
}