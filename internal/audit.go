package internal

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// newUUID returns a v7 UUID string (time-ordered) using google/uuid.
func newUUID() string {
	id, _ := uuid.NewV7()
	return id.String()
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