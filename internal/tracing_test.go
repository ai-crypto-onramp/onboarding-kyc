package internal

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInstallTracerNoEndpoint(t *testing.T) {
	// No OTEL_EXPORTER_OTLP_ENDPOINT -> no-op provider, no error.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	sd, err := installTracer(context.Background(), logger)
	if err != nil {
		t.Fatalf("installTracer: %v", err)
	}
	if sd == nil {
		t.Fatal("expected shutdown fn")
	}
	if err := sd(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Reset global so other tests can install again.
	tracerMu.Lock()
	tracerShutdown = nil
	tracerMu.Unlock()
}

func TestSpanMiddlewareWrapsRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := spanMiddleware(mux)
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestStartSpanReturnsSpan(t *testing.T) {
	_, span := startSpan(context.Background(), "test.span")
	if span == nil {
		t.Fatal("expected span")
	}
	span.End()
}

func TestRecordSpanErrorNoOp(t *testing.T) {
	// nil error -> no-op
	recordSpanError(context.Background(), nil)
	// non-nil with no active span -> no-op (no panic)
	recordSpanError(context.Background(), errFoo)
}

type errFooType struct{}

func (errFooType) Error() string { return "foo" }

var errFoo = errFooType{}

func TestShutdownTracerNoProvider(t *testing.T) {
	tracerMu.Lock()
	tracerShutdown = nil
	tracerMu.Unlock()
	if err := shutdownTracer(context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}