package internal

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// RetentionSweeper periodically hard-deletes (and redacts) documents and
// liveness sessions whose retention_until timestamp is in the past. It
// operates on the DocumentRepo and LivenessRepo; in a production deployment
// the same loop would issue DELETE queries against the DB and expire objects
// in object storage.
type RetentionSweeper struct {
	docs     DocumentRepo
	liveness LivenessRepo
	audit    *AuditLog
	logger   *slog.Logger
	now      func() time.Time
	stop     chan struct{}
	mu       sync.Mutex
	running  bool
}

// NewRetentionSweeper builds a sweeper over the given stores.
func NewRetentionSweeper(docs DocumentRepo, liveness LivenessRepo, audit *AuditLog, logger *slog.Logger) *RetentionSweeper {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return &RetentionSweeper{
		docs:     docs,
		liveness: liveness,
		audit:    audit,
		logger:   logger,
		now:      time.Now,
		stop:     make(chan struct{}),
	}
}

// Sweep performs one sweep pass, deleting expired documents and liveness
// sessions. Returns the number of documents and sessions removed.
func (r *RetentionSweeper) Sweep(ctx context.Context, now time.Time) (docsRemoved, sessionsRemoved int) {
	_, span := startSpan(ctx, "retention.Sweep")
	defer span.End()
	docsRemoved = r.docs.SweepExpired(now)
	sessionsRemoved = r.liveness.SweepExpired(now)
	if docsRemoved > 0 && r.audit != nil {
		r.audit.Record("retention", "retention_sweep", "system", map[string]any{
			"documents_removed": docsRemoved,
			"liveness_removed":  sessionsRemoved,
		})
	}
	r.logger.Info("retention sweep complete",
		"documents_removed", docsRemoved, "liveness_removed", sessionsRemoved)
	return docsRemoved, sessionsRemoved
}

// Start launches a background goroutine that sweeps at the given interval.
// The interval defaults to 1h when RETENTION_SWEEP_INTERVAL is unset; pass the
// configured duration explicitly for tests.
func (r *RetentionSweeper) Start(interval time.Duration) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	if interval <= 0 {
		interval = envDurationDefault("RETENTION_SWEEP_INTERVAL", time.Hour)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.Sweep(context.Background(), r.now())
			}
		}
	}()
}

// Stop terminates the background sweeper.
func (r *RetentionSweeper) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	close(r.stop)
	r.running = false
}

// retentionInterval returns the configured sweep interval from env, defaulting
// to 1h.
func retentionInterval() time.Duration {
	return envDurationDefault("RETENTION_SWEEP_INTERVAL", time.Hour)
}