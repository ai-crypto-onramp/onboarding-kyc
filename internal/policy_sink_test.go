package internal

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPolicyEventSinkSyncSuccess(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		var evt PolicyEvent
		_ = json.NewDecoder(r.Body).Decode(&evt)
		if evt.Type != "state_transition" {
			t.Errorf("expected state_transition, got %s", evt.Type)
		}
		if evt.ApplicationID != "app1" {
			t.Errorf("expected app1, got %s", evt.ApplicationID)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("POLICY_RISK_ENGINE_URL", srv.URL)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	sink := NewPolicyEventSink(logger)
	defer sink.Stop()
	sink.RecordTransition("app1", StateStarted, StateDocumentsUploaded, "system", "docs uploaded")
	// give the async drainer (if any) no reason to fire; sync should suffice
	if got := received.Load(); got != 1 {
		t.Fatalf("expected 1 received, got %d", got)
	}
}

func TestPolicyEventSinkAsyncFallbackOnFailure(t *testing.T) {
	// First server always fails 5xx to force async fallback.
	var failCalls atomic.Int32
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	// After the sync failure the event is enqueued; the async drainer retries
	// with backoff. To make the test deterministic, swap the endpoint to a
	// healthy server after the first sync failure. We do this by intercepting
	// the URL via env at construction; the sink reads endpoint once. Instead,
	// verify the event reaches the queue by checking that the failSrv gets
	// hit again (async retry) within a reasonable window.
	defer failSrv.Close()
	t.Setenv("POLICY_RISK_ENGINE_URL", failSrv.URL)
	// Cap the queue so the enqueue is non-blocking.
	t.Setenv("POLICY_EVENT_QUEUE_CAP", "16")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	sink := NewPolicyEventSink(logger)
	defer sink.Stop()
	sink.RecordTransition("app2", StateStarted, StatePass, "system", "final")
	// Wait for at least 2 total calls (1 sync + >=1 async retry).
	deadline := time.Now().Add(5 * time.Second)
	for failCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if failCalls.Load() < 2 {
		t.Fatalf("expected >=2 calls (sync+async), got %d", failCalls.Load())
	}
}

func TestPolicyEventSinkNoEndpointNoOp(t *testing.T) {
	// No POLICY_RISK_ENGINE_URL set -> sink is a no-op and does not start a
	// drainer. RecordTransition must not panic or block.
	sink := NewPolicyEventSink(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer sink.Stop()
	sink.RecordTransition("app3", StatePass, StateFail, "x", "y")
}

func TestPolicyEventSinkPublishDecision(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		var evt PolicyEvent
		_ = json.NewDecoder(r.Body).Decode(&evt)
		if evt.Type != "decision" {
			t.Errorf("expected decision, got %s", evt.Type)
		}
		if evt.Outcome != "pass" {
			t.Errorf("expected pass, got %s", evt.Outcome)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("POLICY_RISK_ENGINE_URL", srv.URL)
	sink := NewPolicyEventSink(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer sink.Stop()
	sink.PublishDecision(context.Background(), "app4", "pass", "clear", "analyst")
	if seen.Load() != 1 {
		t.Fatalf("expected 1, got %d", seen.Load())
	}
}

func TestCompositeEventSinkFansOut(t *testing.T) {
	var primary, secondary atomic.Int32
	c := &compositeEventSink{
		primary:   eventSinkFunc(func(appID string, from, to State, actor, reason string) { primary.Add(1) }),
		secondary: eventSinkFunc(func(appID string, from, to State, actor, reason string) { secondary.Add(1) }),
	}
	c.RecordTransition("a", StateStarted, StatePass, "x", "y")
	if primary.Load() != 1 || secondary.Load() != 1 {
		t.Fatalf("expected 1/1, got %d/%d", primary.Load(), secondary.Load())
	}
}

// eventSinkFunc is a test helper that adapts a function to EventSink.
type eventSinkFunc func(appID string, from, to State, actor, reason string)

func (f eventSinkFunc) RecordTransition(appID string, from, to State, actor, reason string) {
	f(appID, from, to, actor, reason)
}