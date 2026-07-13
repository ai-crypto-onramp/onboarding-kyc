package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListSyncJobSyncOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LIST_SYNC_DIR", dir)
	client := NewInMemoryScreeningClient()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	job := NewListSyncJob(client, logger)
	snap, err := job.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.Names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(snap.Names))
	}
	// verify file written
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 snapshot file, got %d", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("expected json file, got %s", entries[0].Name())
	}
	if job.LastSnapshot() == nil {
		t.Fatal("expected last snapshot set")
	}
}

func TestListSyncJobStartStopDisabled(t *testing.T) {
	// No LIST_SYNC_INTERVAL set -> Start is a no-op.
	client := NewInMemoryScreeningClient()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	job := NewListSyncJob(client, logger)
	job.Start()
	time.Sleep(30 * time.Millisecond)
	job.Stop()
}

func TestListSyncJobStartEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LIST_SYNC_DIR", dir)
	t.Setenv("LIST_SYNC_INTERVAL", "20ms")
	client := NewInMemoryScreeningClient()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	job := NewListSyncJob(client, logger)
	job.Start()
	time.Sleep(80 * time.Millisecond)
	job.Stop()
	// At least one snapshot file should exist (immediate + ticker fires).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) < 1 {
		t.Fatalf("expected >=1 snapshot, got %d", len(entries))
	}
}

func TestListSyncJobPersistFailure(t *testing.T) {
	// Point dir at a path that cannot be created (a file).
	tmp := t.TempDir()
	badDir := filepath.Join(tmp, "file")
	if err := os.WriteFile(badDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIST_SYNC_DIR", badDir)
	client := NewInMemoryScreeningClient()
	job := NewListSyncJob(client, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if _, err := job.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected persist error")
	}
}

func TestInMemoryScreeningClientNames(t *testing.T) {
	c := NewInMemoryScreeningClient()
	names := c.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2, got %d", len(names))
	}
	c.SetNames([]string{"NEW"})
	got := c.Names()
	if len(got) != 1 || got[0] != "NEW" {
		t.Fatalf("expected [NEW], got %v", got)
	}
}