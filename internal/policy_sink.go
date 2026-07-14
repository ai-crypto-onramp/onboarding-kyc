package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// PolicyEvent is the payload published to the Policy/Risk Engine. It carries
// either a state transition or a final KYC decision.
type PolicyEvent struct {
	Type         string    `json:"type"` // "state_transition" | "decision"
	ApplicationID string   `json:"application_id"`
	From         string    `json:"from,omitempty"`
	To           string    `json:"to,omitempty"`
	Outcome      string    `json:"outcome,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Actor        string    `json:"actor,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// PolicyEventSink publishes KYC state transitions and decisions to the
// Policy/Risk Engine via HTTP. It tries a synchronous POST first; on failure
// it enqueues the event onto a bounded async queue that is drained by a
// background worker with retry.
type PolicyEventSink struct {
	endpoint string
	http     *http.Client
	tracer   trace.Tracer
	logger   *slog.Logger

	queueCap int
	queue    chan PolicyEvent
	wg       sync.WaitGroup
	stop     chan struct{}
}

// NewPolicyEventSink creates a sink targeting POLICY_RISK_ENGINE_URL. If the
// env var is empty the sink is a no-op (events are dropped with a debug log).
// queueCap bounds the async fallback queue (default 1024).
func NewPolicyEventSink(logger *slog.Logger) *PolicyEventSink {
	endpoint := os.Getenv("POLICY_RISK_ENGINE_URL")
	cap := envIntDefault("POLICY_EVENT_QUEUE_CAP", 1024)
	s := &PolicyEventSink{
		endpoint: endpoint,
		http: &http.Client{
			Timeout: envDurationDefault("POLICY_RISK_ENGINE_TIMEOUT", 5*time.Second),
		},
		tracer:   tracer(),
		logger:   logger,
		queueCap: cap,
		queue:    make(chan PolicyEvent, cap),
		stop:     make(chan struct{}),
	}
	if endpoint != "" {
		s.wg.Add(1)
		go s.drain()
	}
	return s
}

// RecordTransition implements EventSink. It publishes a state_transition event
// synchronously; on failure it enqueues the event for async retry.
func (p *PolicyEventSink) RecordTransition(appID string, from, to State, actor, reason string) {
	evt := PolicyEvent{
		Type:          "state_transition",
		ApplicationID: appID,
		From:          string(from),
		To:            string(to),
		Actor:         actor,
		Reason:        reason,
		OccurredAt:    time.Now(),
	}
	p.publish(context.Background(), evt)
}

// PublishDecision publishes a final KYC decision event.
func (p *PolicyEventSink) PublishDecision(ctx context.Context, appID, outcome, reason, actor string) {
	evt := PolicyEvent{
		Type:          "decision",
		ApplicationID: appID,
		Outcome:       outcome,
		Reason:        reason,
		Actor:         actor,
		OccurredAt:    time.Now(),
	}
	p.publish(ctx, evt)
}

// publish attempts a synchronous POST; on failure it enqueues for async.
func (p *PolicyEventSink) publish(ctx context.Context, evt PolicyEvent) {
	if p.endpoint == "" {
		return
	}
	ctx, span := p.tracer.Start(ctx, "policy.publish", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	if err := p.post(ctx, evt); err == nil {
		return
	} else {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	// Fallback: enqueue for async delivery (non-blocking; drop if full).
	select {
	case p.queue <- evt:
	default:
		p.logger.Warn("policy event queue full, dropping event",
			"application_id", evt.ApplicationID, "type", evt.Type)
	}
}

// post sends a single event synchronously.
func (p *PolicyEventSink) post(ctx context.Context, evt PolicyEvent) error {
	b, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("policy engine returned status %d", resp.StatusCode)
	}
	return nil
}

// drain consumes the async queue, retrying events with backoff.
func (p *PolicyEventSink) drain() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stop:
			return
		case evt := <-p.queue:
			backoff := time.Second
			for attempt := 1; attempt <= 5; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := p.post(ctx, evt); err == nil {
					cancel()
					break
				} else {
					p.logger.Warn("policy async publish failed",
						"attempt", attempt, "error", err, "application_id", evt.ApplicationID)
				}
				cancel()
				if attempt == 5 {
					p.logger.Error("policy event delivery exhausted retries, dropping",
						"application_id", evt.ApplicationID, "type", evt.Type)
					break
				}
				select {
				case <-p.stop:
					return
				case <-time.After(backoff):
				}
				backoff *= 2
			}
		}
	}
}

// Stop shuts down the async drainer, flushing queued events best-effort.
func (p *PolicyEventSink) Stop() {
	close(p.stop)
	p.wg.Wait()
}

// ErrPolicyEngineUnreachable is returned by callers that need a typed error.
var ErrPolicyEngineUnreachable = errors.New("policy engine unreachable")

// compositeEventSink fans out RecordTransition calls to a primary sink (e.g.
// the Policy/Risk Engine) and a secondary sink (the in-memory audit log) so
// both subsystems receive every transition.
type compositeEventSink struct {
	primary   EventSink
	secondary EventSink
}

func (c *compositeEventSink) RecordTransition(appID string, from, to State, actor, reason string) {
	c.primary.RecordTransition(appID, from, to, actor, reason)
	c.secondary.RecordTransition(appID, from, to, actor, reason)
}