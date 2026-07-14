package internal

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// ListSnapshot is a persisted snapshot of sanctions/PEP lists downloaded from
// the screening provider for offline matching. It is written to a local file
// under the path configured by LIST_SYNC_DIR (default ./var/lists).
type ListSnapshot struct {
	Source      string    `json:"source"`
	GeneratedAt time.Time `json:"generated_at"`
	Names       []string  `json:"names"`
}

// ListSyncJob periodically fetches sanctions/PEP lists snapshots from the
// screening vendor and persists them to a local directory for offline
// matching. It is gated behind LIST_SYNC_INTERVAL; when unset (the default)
// the job does not start.
type ListSyncJob struct {
	client   ScreeningClient
	interval time.Duration
	dir      string
	logger   *slog.Logger
	now      func() time.Time
	stop     chan struct{}
	mu       sync.Mutex
	running  bool
	lastSnap *ListSnapshot
}

// NewListSyncJob builds a list sync job. interval defaults to
// LIST_SYNC_INTERVAL (parsed as a duration); if unset or <= 0 the job is
// disabled and Start is a no-op.
func NewListSyncJob(client ScreeningClient, logger *slog.Logger) *ListSyncJob {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	dir := os.Getenv("LIST_SYNC_DIR")
	if dir == "" {
		dir = "var/lists"
	}
	return &ListSyncJob{
		client:   client,
		interval: envDurationDefault("LIST_SYNC_INTERVAL", 0),
		dir:      dir,
		logger:   logger,
		now:      time.Now,
		stop:     make(chan struct{}),
	}
}

// Start launches the background sync loop if an interval is configured.
func (j *ListSyncJob) Start() {
	j.mu.Lock()
	if j.running || j.interval <= 0 {
		j.mu.Unlock()
		return
	}
	j.running = true
	j.mu.Unlock()
	go func() {
		ticker := time.NewTicker(j.interval)
		defer ticker.Stop()
		// Run once immediately on start.
		j.SyncOnce(context.Background())
		for {
			select {
			case <-j.stop:
				return
			case <-ticker.C:
				j.SyncOnce(context.Background())
			}
		}
	}()
}

// Stop terminates the background sync loop.
func (j *ListSyncJob) Stop() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.running {
		return
	}
	close(j.stop)
	j.running = false
}

// SyncOnce fetches the latest list snapshot from the screening provider and
// persists it to disk. For the in-memory screening client the "fetch" is a
// no-op that returns the seed list; a real provider would call its /lists
// endpoint. Returns the snapshot and any error.
func (j *ListSyncJob) SyncOnce(ctx context.Context) (*ListSnapshot, error) {
	_, span := startSpan(ctx, "list_sync.SyncOnce")
	defer span.End()
	names, err := j.fetchNames(ctx)
	if err != nil {
		j.logger.Warn("list sync fetch failed", "error", err)
		return nil, err
	}
	snap := &ListSnapshot{
		Source:      "screening_provider",
		GeneratedAt: j.now(),
		Names:       names,
	}
	if err := j.persist(snap); err != nil {
		j.logger.Warn("list sync persist failed", "error", err)
		return nil, err
	}
	j.mu.Lock()
	j.lastSnap = snap
	j.mu.Unlock()
	j.logger.Info("list sync complete", "names", len(names))
	return snap, nil
}

// fetchNames retrieves the list of sanctioned/PEP names from the provider.
// For the InMemoryScreeningClient we expose the seed list via a Screen call
// against a sentinel name that matches nothing, then return the built-in
// names. Real providers would call a dedicated lists endpoint.
func (j *ListSyncJob) fetchNames(ctx context.Context) ([]string, error) {
	if c, ok := j.client.(*InMemoryScreeningClient); ok {
		return c.Names(), nil
	}
	// Fallback: screen a sentinel and surface no hits; this keeps the loop
	// exercising the client interface for non-stub providers.
	if _, err := j.client.Screen(ctx, "ZZZ NO MATCH"); err != nil {
		return nil, err
	}
	return nil, nil
}

// persist writes the snapshot to <dir>/snapshot-<unix>.json.
func (j *ListSyncJob) persist(snap *ListSnapshot) error {
	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		return err
	}
	path := j.dir + "/snapshot-" + strconv.FormatInt(snap.GeneratedAt.Unix(), 10) + ".json"
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LastSnapshot returns the most recently synced snapshot, or nil.
func (j *ListSyncJob) LastSnapshot() *ListSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastSnap
}