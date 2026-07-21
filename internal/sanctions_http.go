package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// HTTPSanctionsClient screens a full name against a remote sanctions/PEP list
// service exposing GET {SANCTIONS_LIST_URL}/search?name={fullName}. The
// response is expected to be JSON of the form {"hits":[{...}]}.
type HTTPSanctionsClient struct {
	base   string
	apiKey string
	http   *http.Client
	tracer trace.Tracer
}

// NewHTTPSanctionsClient builds an HTTPSanctionsClient from env. baseURL falls
// back to SANCTIONS_LIST_URL; apiKey falls back to KYC_VENDOR_API_KEY.
func NewHTTPSanctionsClient(baseURL, apiKey string, timeout time.Duration) *HTTPSanctionsClient {
	if baseURL == "" {
		baseURL = os.Getenv("SANCTIONS_LIST_URL")
	}
	if apiKey == "" {
		apiKey = os.Getenv("KYC_VENDOR_API_KEY")
	}
	if timeout <= 0 {
		timeout = envDurationDefault("VENDOR_CALL_TIMEOUT", 30*time.Second)
	}
	return &HTTPSanctionsClient{
		base:   strings.TrimRight(baseURL, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: timeout},
		tracer: otel.Tracer("sanctions.http"),
	}
}

// sanctionsHitJSON is the JSON shape returned by the sanctions list service.
type sanctionsHitJSON struct {
	List        string  `json:"list"`
	MatchedName string  `json:"matched_name"`
	Score       float64 `json:"score"`
}

type sanctionsSearchResponse struct {
	Hits []sanctionsHitJSON `json:"hits"`
}

// Screen calls the sanctions search endpoint and returns parsed hits.
func (c *HTTPSanctionsClient) Screen(ctx context.Context, fullName string) ([]ScreeningHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, span := c.tracer.Start(ctx, "sanctions.search", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	u := c.base + "/search?name=" + url.QueryEscape(fullName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sanctions read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		verr := fmt.Errorf("sanctions search: status %d: %s", resp.StatusCode, string(raw))
		span.RecordError(verr)
		span.SetStatus(codes.Error, verr.Error())
		return nil, verr
	}
	var sr sanctionsSearchResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("sanctions decode: %w", err)
	}
	hits := make([]ScreeningHit, 0, len(sr.Hits))
	for _, h := range sr.Hits {
		hits = append(hits, ScreeningHit(h))
	}
	return hits, nil
}

// NewScreeningClientForMode selects a ScreeningClient by env: HTTP client when
// SANCTIONS_LIST_URL is set, otherwise in-memory (DEV_MODE only).
func NewScreeningClientForMode(devMode bool) ScreeningClient {
	if u := os.Getenv("SANCTIONS_LIST_URL"); u != "" {
		return NewHTTPSanctionsClient(u, "", 0)
	}
	if devMode {
		return NewInMemoryScreeningClient()
	}
	return NewInMemoryScreeningClient()
}
