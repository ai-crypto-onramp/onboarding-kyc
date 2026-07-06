package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrateRoundTrip(t *testing.T) {
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		t.Skip("DB_URL not set; skipping live migration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DB_URL: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()

	// Ensure a clean slate and the schema_migrations table.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS audit_events, webhook_events, kyc_decisions, sanctions_hits, liveness_sessions, documents, kyc_applications, schema_migrations CASCADE`); err != nil {
		t.Fatalf("drop tables: %v", err)
	}

	if err := MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	wantTables := []string{
		"kyc_applications",
		"documents",
		"liveness_sessions",
		"sanctions_hits",
		"kyc_decisions",
		"webhook_events",
		"audit_events",
		"schema_migrations",
	}
	for _, table := range wantTables {
		var exists bool
		err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q not created by MigrateUp", table)
		}
	}

	// Round-trip: down to 0 should remove all application tables.
	if err := MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	for _, table := range wantTables {
		if table == "schema_migrations" {
			continue
		}
		var exists bool
		err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s after down: %v", table, err)
		}
		if exists {
			t.Errorf("table %q still present after MigrateDown", table)
		}
	}
}

func TestOpenPingsAndMigrates(t *testing.T) {
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		t.Skip("DB_URL not set; skipping live Open test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := LoadConfig()
	cfg.DBURL = dbURL
	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	if err := NewHealthChecker(pool).Check(ctx); err != nil {
		t.Fatalf("HealthChecker after Open: %v", err)
	}
}