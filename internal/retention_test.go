package internal

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestDocumentStoreSweepExpired(t *testing.T) {
	d := NewDocumentStore()
	now := time.Now()
	d.Add("a1", Document{
		ID:             "d1",
		ApplicationID:  "a1",
		Type:           "ID_FRONT",
		Content:        []byte("secret"),
		UploadedAt:     now.Add(-400 * 24 * time.Hour),
		RetentionUntil: now.Add(-1 * time.Hour), // expired
	})
	d.Add("a1", Document{
		ID:             "d2",
		ApplicationID:  "a1",
		Type:           "SELFIE",
		Content:        []byte("keep"),
		UploadedAt:     now,
		RetentionUntil: now.Add(365 * 24 * time.Hour), // not expired
	})
	removed := d.SweepExpired(now)
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	remaining := d.List("a1")
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(remaining))
	}
	if remaining[0].ID != "d2" {
		t.Fatalf("expected d2 kept, got %s", remaining[0].ID)
	}
}

func TestLivenessStoreSweepExpired(t *testing.T) {
	l := NewLivenessStore()
	now := time.Now()
	l.Add("a1", LivenessSession{
		ID:             "s1",
		ApplicationID:  "a1",
		RetentionUntil: now.Add(-time.Hour), // expired
	})
	l.Add("a1", LivenessSession{
		ID:             "s2",
		ApplicationID:  "a1",
		RetentionUntil: now.Add(time.Hour), // not expired
	})
	removed := l.SweepExpired(now)
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	sess, ok := l.Latest("a1")
	if !ok {
		t.Fatal("expected a session to remain")
	}
	if sess.ID != "s2" {
		t.Fatalf("expected s2, got %s", sess.ID)
	}
}

func TestRetentionSweeperSweep(t *testing.T) {
	d := NewDocumentStore()
	l := NewLivenessStore()
	audit := NewAuditLog()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewRetentionSweeper(d, l, audit, logger)
	now := time.Now()
	d.Add("a1", Document{
		ID:             "d1",
		ApplicationID:  "a1",
		Type:           "ID_FRONT",
		Content:        []byte("x"),
		RetentionUntil: now.Add(-time.Hour),
	})
	l.Add("a1", LivenessSession{
		ID:             "s1",
		ApplicationID:  "a1",
		RetentionUntil: now.Add(-time.Hour),
	})
	dr, sr := s.Sweep(context.Background(), now)
	if dr != 1 || sr != 1 {
		t.Fatalf("expected 1/1, got %d/%d", dr, sr)
	}
	if len(audit.List()) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(audit.List()))
	}
}

func TestRetentionSweeperStartStop(t *testing.T) {
	d := NewDocumentStore()
	l := NewLivenessStore()
	audit := NewAuditLog()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewRetentionSweeper(d, l, audit, logger)
	s.Start(10 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	s.Stop()
}

func TestRetentionIntervalDefault(t *testing.T) {
	if got := retentionInterval(); got != time.Hour {
		t.Fatalf("expected 1h default, got %v", got)
	}
	t.Setenv("RETENTION_SWEEP_INTERVAL", "30s")
	if got := retentionInterval(); got != 30*time.Second {
		t.Fatalf("expected 30s, got %v", got)
	}
}