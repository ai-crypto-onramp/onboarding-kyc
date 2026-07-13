package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// httpVendorClient is a minimal HTTP-based vendor client that wraps outbound
// calls with the retry layer and OTel spans. It is provider-agnostic; concrete
// vendors (Onfido/Sumsub) would embed or wrap it. The StubVendorClient remains
// the default for dev/test.
type httpVendorClient struct {
	name    string
	base    string
	apiKey  string
	http    *http.Client
	retry   retryConfig
	tracer  trace.Tracer
}

// newHTTPVendorClient builds an HTTP vendor client from env. baseURL and
// apiKey come from VENDOR_API_BASE and VENDOR_API_KEY unless overridden by
// the caller.
func newHTTPVendorClient(name, baseURL, apiKey string, timeout time.Duration) *httpVendorClient {
	if baseURL == "" {
		baseURL = os.Getenv("VENDOR_API_BASE")
	}
	if apiKey == "" {
		apiKey = os.Getenv("VENDOR_API_KEY")
	}
	if timeout <= 0 {
		timeout = envDurationDefault("VENDOR_CALL_TIMEOUT", 30*time.Second)
	}
	return &httpVendorClient{
		name:   name,
		base:   baseURL,
		apiKey: apiKey,
		http: &http.Client{
			Timeout: timeout,
		},
		retry:  defaultRetryConfig(),
		tracer: otel.Tracer("vendor"),
	}
}

// doRequest performs an HTTP request with retry + tracing. The returned
// response body is NOT closed; the caller must close it. On a non-2xx
// response that is not retried (or after exhausting retries), the response is
// still returned along with a nil error; callers should inspect StatusCode.
func (c *httpVendorClient) doRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	ctx, span := c.tracer.Start(ctx, "vendor.http."+method, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	url := c.base + path
	do := func(attempt int) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Token token="+c.apiKey)
		}
		return c.http.Do(req)
	}
	resp, err := doWithRetry(ctx, c.retry, do)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("status %d", resp.StatusCode))
	}
	return resp, nil
}

// closeBody is a helper that closes the response body, used by callers of
// doRequest after reading the body.
func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// readBody reads and closes the response body, returning its bytes.
func readBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	defer closeBody(resp)
	return io.ReadAll(resp.Body)
}