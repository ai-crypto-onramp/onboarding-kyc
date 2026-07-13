package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryableStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{200, false}, {400, false}, {401, false}, {403, false}, {404, false},
		{408, true}, {429, true}, {500, true}, {502, true}, {503, true}, {599, true},
	}
	for _, c := range cases {
		if got := retryableStatus(c.status); got != c.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestDoWithRetrySucceedsFirstTry(t *testing.T) {
	var calls atomic.Int32
	cfg := retryConfig{maxAttempts: 3, baseDelay: time.Millisecond, maxDelay: 10 * time.Millisecond}
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
		t.Fatalf("calls: %d", calls.Load())
	}
}

func TestDoWithRetryRetries5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	cfg := retryConfig{maxAttempts: 4, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	resp, err := doWithRetry(context.Background(), cfg, func(attempt int) (*http.Response, error) {
		n := calls.Add(1)
		if n < 3 {
			return &http.Response{StatusCode: 500, Body: http.NoBody}, nil
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoWithRetryDoesNotRetry4xx(t *testing.T) {
	var calls atomic.Int32
	cfg := retryConfig{maxAttempts: 5, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	resp, err := doWithRetry(context.Background(), cfg, func(attempt int) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 400, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", calls.Load())
	}
}

func TestDoWithRetryExhaustsAttempts(t *testing.T) {
	var calls atomic.Int32
	cfg := retryConfig{maxAttempts: 3, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	resp, err := doWithRetry(context.Background(), cfg, func(attempt int) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 503, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoWithRetryContextCancelled(t *testing.T) {
	cfg := retryConfig{maxAttempts: 5, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := doWithRetry(ctx, cfg, func(attempt int) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: http.NoBody}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDoWithRetryTransportErrorRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// First simulate transport errors by pointing at a server we close.
	srv.Close()
	cfg := retryConfig{maxAttempts: 2, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	var calls atomic.Int32
	_, err := doWithRetry(context.Background(), cfg, func(attempt int) (*http.Response, error) {
		calls.Add(1)
		// Real HTTP request to closed server -> transport error.
		req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Listener.Addr().String(), nil)
		return http.DefaultClient.Do(req)
	})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestHTTPVendorClientRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := newHTTPVendorClient("test", srv.URL, "k", 5*time.Second)
	c.retry = retryConfig{maxAttempts: 3, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	resp, err := c.doRequest(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"ok":true}` {
		t.Fatalf("body: %s", b)
	}
}