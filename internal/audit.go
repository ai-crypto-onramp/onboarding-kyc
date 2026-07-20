package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

const AuditTopic = "audit.v1"

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

// AuditLog is an append-only audit log. Events are recorded in memory and,
// when a Kafka sink is configured, also published to the `audit.v1` topic
// in the canonical envelope (see .github/contracts/asyncapi/audit/v1/asyncapi.yaml).
type AuditLog struct {
	mu     sync.Mutex
	events []AuditEvent
	now    func() time.Time
	sink   *kafka.Writer
}

// NewAuditLog creates a new in-memory audit log. The Kafka sink is
// configured via AuditLog.WithKafkaSink or AuditLog.NewFromEnv.
func NewAuditLog() *AuditLog {
	return &AuditLog{now: time.Now}
}

// WithKafkaSink wires a Kafka writer targeting the `audit.v1` topic.
func (a *AuditLog) WithKafkaSink(brokers []string) error {
	if len(brokers) == 0 {
		return fmt.Errorf("audit kafka: no brokers provided")
	}
	a.sink = &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        AuditTopic,
		Balancer:     &kafka.LeastBytes{},
		BatchTimeout: 10 * time.Millisecond,
		RequiredAcks: kafka.RequireAll,
	}
	return nil
}

// Close flushes and closes the Kafka sink, if configured.
func (a *AuditLog) Close() error {
	if a.sink == nil {
		return nil
	}
	return a.sink.Close()
}

// NewAuditLogFromEnv builds an AuditLog with an optional Kafka sink based on
// KAFKA_BROKERS. When KAFKA_BROKERS is unset and DEV_MODE=1, the log is
// in-memory only; otherwise it is fatal.
func NewAuditLogFromEnv() *AuditLog {
	a := NewAuditLog()
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		if devMode() {
			slog.Warn("KAFKA_BROKERS unset and DEV_MODE=1; audit events recorded in-memory only")
			return a
		}
		slog.Error("KAFKA_BROKERS unset and DEV_MODE not set; cannot start audit producer")
		os.Exit(1)
	}
	if err := a.WithKafkaSink(splitCSV(brokers)); err != nil {
		if devMode() {
			slog.Warn("audit kafka init failed (DEV_MODE), recording in-memory only", "error", err)
			return a
		}
		slog.Error("audit kafka init failed", "error", err)
		os.Exit(1)
	}
	return a
}

// Record appends an audit event and, when a sink is configured, publishes it.
func (a *AuditLog) Record(aggregate, action, actor string, payload map[string]any) {
	a.mu.Lock()
	ev := AuditEvent{
		ID:         newUUID(),
		Aggregate:  aggregate,
		Action:     action,
		Actor:      actor,
		Payload:    payload,
		OccurredAt: a.now(),
	}
	a.events = append(a.events, ev)
	a.mu.Unlock()
	if a.sink != nil {
		a.publish(ev)
	}
}

func (a *AuditLog) publish(ev AuditEvent) {
	payloadBytes, err := json.Marshal(ev)
	if err != nil {
		return
	}
	sum := sha256.Sum256(payloadBytes)
	payloadHash := "sha256:" + hex.EncodeToString(sum[:])
	envelope := map[string]any{
		"schema_version": "1",
		"id":              ev.ID,
		"ts":              ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		"source_service":  "onboarding-kyc",
		"actor_id":        coalesceStr(ev.Actor, "onboarding-kyc"),
		"action":          ev.Action,
		"target_type":     "kyc_application",
		"target_id":       ev.Aggregate,
		"payload_hash":    payloadHash,
		"payload":         ev.Payload,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return
	}
	_ = a.sink.WriteMessages(nil, kafka.Message{
		Key:   []byte(ev.ID),
		Value: body,
	})
}

func coalesceStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func devMode() bool {
	v := os.Getenv("DEV_MODE")
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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