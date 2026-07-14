package internal

import (
	"context"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"
)

// retryConfig governs vendor HTTP call retries.
type retryConfig struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
}

// defaultRetryConfig returns a retryConfig populated from environment with
// sane defaults: VENDOR_MAX_ATTEMPTS (default 5), VENDOR_RETRY_BASE_DELAY
// (default 100ms), VENDOR_RETRY_MAX_DELAY (default 2s).
func defaultRetryConfig() retryConfig {
	return retryConfig{
		maxAttempts: envIntDefault("VENDOR_MAX_ATTEMPTS", 5),
		baseDelay:   envDurationDefault("VENDOR_RETRY_BASE_DELAY", 100*time.Millisecond),
		maxDelay:    envDurationDefault("VENDOR_RETRY_MAX_DELAY", 2*time.Second),
	}
}

func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDurationDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// retryableStatus reports whether an HTTP status code should be retried.
// Retry on 5xx, 408 (Request Timeout), 429 (Too Many Requests). Do not retry
// other 4xx.
func retryableStatus(status int) bool {
	switch {
	case status >= 500 && status <= 599:
		return true
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return true
	}
	return false
}

// doWithRetry executes fn (an HTTP round-trip attempt) with exponential
// backoff and full jitter. fn must return either a non-nil response or an
// error; the response status is inspected for retry decisions. The returned
// response (if any) is the last one seen; the caller is responsible for
// closing its body.
func doWithRetry(ctx context.Context, cfg retryConfig, fn func(attempt int) (*http.Response, error)) (*http.Response, error) {
	if cfg.maxAttempts < 1 {
		cfg.maxAttempts = 1
	}
	var (
		resp *http.Response
		err  error
	)
	for attempt := 1; attempt <= cfg.maxAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		resp, err = fn(attempt)
		if err == nil && !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// Close the response body of any retried attempt to avoid leaks.
		if resp != nil && retryableStatus(resp.StatusCode) {
			resp.Body.Close()
		}
		// Non-retryable HTTP status: return immediately.
		if err == nil && !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// Stop if this was the last attempt.
		if attempt == cfg.maxAttempts {
			break
		}
		// Compute backoff with full jitter: uniform in [0, min(maxDelay,
		// baseDelay * 2^(attempt-1))].
		upper := cfg.baseDelay * (1 << (attempt - 1))
		if upper > cfg.maxDelay || upper <= 0 {
			upper = cfg.maxDelay
		}
		delay := time.Duration(rand.Int64N(int64(upper)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return resp, err
}