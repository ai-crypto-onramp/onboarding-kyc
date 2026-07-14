package internal

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// newUUID returns a v4 UUID string using crypto/rand.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// extremely unlikely; fall back to time-based
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// AuditEvent is an append-only audit log entry.
type AuditEvent struct {
	ID          string    `json:"id"`
	Aggregate   string    `json:"aggregate"`
	Action      string    `json:"action"`
	Actor       string    `json:"actor"`
	Payload     map[string]any `json:"payload,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// AuditLog is an in-memory append-only audit log.
type AuditLog struct {
	mu     sync.Mutex
	events []AuditEvent
	now    func() time.Time
}

// NewAuditLog creates a new in-memory audit log.
func NewAuditLog() *AuditLog {
	return &AuditLog{now: time.Now}
}

// Record appends an audit event.
func (a *AuditLog) Record(aggregate, action, actor string, payload map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, AuditEvent{
		ID:         newUUID(),
		Aggregate:  aggregate,
		Action:     action,
		Actor:      actor,
		Payload:    payload,
		OccurredAt: a.now(),
	})
}

// RecordTransition records a state transition audit event.
func (a *AuditLog) RecordTransition(appID string, from, to State, actor, reason string) {
	globalMetrics.transitionsTotal.Add(1)
	a.Record(appID, "state_transition", actor, map[string]any{
		"from":   string(from),
		"to":     string(to),
		"reason": reason,
	})
}

// List returns a copy of all audit events.
func (a *AuditLog) List() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEvent, len(a.events))
	copy(out, a.events)
	return out
}