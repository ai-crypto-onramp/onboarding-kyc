package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebhookStore provides idempotent dedup of webhook event ids.
type WebhookStore struct {
	mu      sync.Mutex
	seen    map[string]struct{}
}

// NewWebhookStore creates a new WebhookStore.
func NewWebhookStore() *WebhookStore {
	return &WebhookStore{seen: make(map[string]struct{})}
}

// Seen marks an event id as processed; returns true if it was already seen.
func (w *WebhookStore) Seen(eventID string) bool {
	if eventID == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[eventID]; ok {
		return true
	}
	w.seen[eventID] = struct{}{}
	return false
}

var (
	errBadSignature = errors.New("invalid webhook signature")
	errStaleTs      = errors.New("stale webhook timestamp")
	errMissingTs    = errors.New("missing webhook timestamp")
	errMissingSig   = errors.New("missing webhook signature")
	errMissingID    = errors.New("missing webhook id")
)

// VerifyWebhook verifies the HMAC-SHA256 signature over the raw body and the
// timestamp skew, returning nil if valid. Signature header format is
// `t=<unix_ts>,v1=<hex_sig>` (Stripe-style) OR a plain `v1=<hex_sig>` header
// paired with a separate X-Webhook-Timestamp header. The function tolerates
// both forms.
func VerifyWebhook(raw []byte, sigHeader, tsHeader string, secret string, tolerance time.Duration) error {
	if secret == "" {
		secret = "dev-webhook-secret"
	}
	if sigHeader == "" {
		return errMissingSig
	}
	ts := tsHeader
	sig := sigHeader
	// Stripe-style: "t=...,v1=..."
	if bytes.Contains([]byte(sigHeader), []byte(",")) && bytes.Contains([]byte(sigHeader), []byte("t=")) {
		var sigPart string
		for _, kv := range bytes.Split([]byte(sigHeader), []byte(",")) {
			parts := bytes.SplitN(kv, []byte("="), 2)
			if len(parts) != 2 {
				continue
			}
			k := string(parts[0])
			v := string(parts[1])
			if k == "t" {
				ts = v
			} else if k == "v1" {
				sigPart = v
			}
		}
		sig = sigPart
	} else {
		// plain "v1=<hex>" or bare hex
		if len(sig) > 3 && sig[:3] == "v1=" {
			sig = sig[3:]
		}
	}
	if ts == "" {
		return errMissingTs
	}
	if sig == "" {
		return errMissingSig
	}
	tsInt, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return errStaleTs
	}
	now := time.Now().Unix()
	if abs(now-tsInt) > int64(tolerance.Seconds()) {
		return errStaleTs
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d", tsInt)))
	mac.Write([]byte("."))
	mac.Write(raw)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(sig))) {
		return errBadSignature
	}
	return nil
}

// abs returns absolute int64.
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// WebhookService handles webhook ingestion, verification, dedup, and
// reconciliation to the state machine.
type WebhookService struct {
	store    *WebhookStore
	repo     ApplicationRepo
	audit    *AuditLog
	vendor   VendorClient
	secret   string
	toleranc time.Duration
	now      func() time.Time
}

// NewWebhookService creates a new WebhookService.
func NewWebhookService(store *WebhookStore, repo ApplicationRepo, audit *AuditLog, vendor VendorClient) *WebhookService {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		secret = "dev-webhook-secret"
	}
	return &WebhookService{
		store:    store,
		repo:     repo,
		audit:    audit,
		vendor:   vendor,
		secret:   secret,
		toleranc: 300 * time.Second,
		now:      time.Now,
	}
}

// IngestResult is the outcome of an ingestion attempt.
type IngestResult struct {
	Accepted   bool
	Duplicate  bool
	EventID    string
	Reason     string
}

// Ingest verifies, dedups, and reconciles a webhook.
func (w *WebhookService) Ingest(ctx context.Context, vendor string, raw []byte, sigHeader, tsHeader, eventID string) IngestResult {
	if err := VerifyWebhook(raw, sigHeader, tsHeader, w.secret, w.toleranc); err != nil {
		if w.audit != nil {
			w.audit.Record(vendor, "webhook_reject", "system", map[string]any{
				"reason": err.Error(),
			})
		}
		return IngestResult{Reason: err.Error()}
	}
	id := eventID
	if id == "" {
		id = "no-id"
	}
	if w.store.Seen(id) {
		if w.audit != nil {
			w.audit.Record(vendor, "webhook_duplicate", "system", map[string]any{"event_id": id})
		}
		return IngestResult{Duplicate: true, EventID: id, Reason: "duplicate"}
	}
	// Parse and reconcile.
	evt, err := w.vendor.ParseWebhook(ctx, vendor, raw)
	if err != nil {
		if w.audit != nil {
			w.audit.Record(vendor, "webhook_reject", "system", map[string]any{
				"reason": "parse: " + err.Error(),
			})
		}
		return IngestResult{Reason: "parse: " + err.Error()}
	}
	if evt.ApplicationID != "" {
		if app, gerr := w.repo.Get(evt.ApplicationID); gerr == nil {
			to := reconcileOutcomeToState(evt.Outcome, app.State)
			if to != "" && CanTransition(app.State, to) {
				if _, uerr := w.repo.UpdateState(app.ID, app.Version, to, "webhook:"+vendor, "webhook event "+id); uerr == nil {
					if w.audit != nil {
						w.audit.Record(app.ID, "webhook_advance", vendor, map[string]any{
							"event_id": id,
							"from":     string(app.State),
							"to":       string(to),
						})
					}
				}
			}
		}
	}
	if w.audit != nil {
		w.audit.Record(vendor, "webhook_accept", "system", map[string]any{"event_id": id})
	}
	return IngestResult{Accepted: true, EventID: id}
}

// reconcileOutcomeToState maps a vendor webhook outcome to the target state.
func reconcileOutcomeToState(outcome string, current State) State {
	switch outcome {
	case "clear", "pass", "approved":
		if CanTransition(current, StatePass) {
			return StatePass
		}
		if CanTransition(current, StateVendorDecision) {
			return StateVendorDecision
		}
	case "fail", "rejected":
		if CanTransition(current, StateFail) {
			return StateFail
		}
	case "consider", "review", "manual_review":
		return StateManualReview
	case "liveness_pass":
		if CanTransition(current, StateLivenessPassed) {
			return StateLivenessPassed
		}
	}
	return ""
}