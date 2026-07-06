package store

import (
	"testing"
)

func TestLoadMigrations(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(migrations) != 7 {
		t.Fatalf("expected 7 migration pairs, got %d", len(migrations))
	}
	want := []int{1, 2, 3, 4, 5, 6, 7}
	for i, m := range migrations {
		if m.Version != want[i] {
			t.Errorf("migration %d: version = %d, want %d", i, m.Version, want[i])
		}
		if m.Up == "" {
			t.Errorf("migration %d: missing Up script", m.Version)
		}
		if m.Down == "" {
			t.Errorf("migration %d: missing Down script", m.Version)
		}
	}
}

func TestLoadMigrationsContainsExpectedTables(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	wantTables := []string{
		"kyc_applications",
		"documents",
		"liveness_sessions",
		"sanctions_hits",
		"kyc_decisions",
		"webhook_events",
		"audit_events",
	}
	for _, table := range wantTables {
		found := false
		for _, m := range migrations {
			if contains(m.Up, table) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no migration creates table %q", table)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("DB_URL", "")
	cfg := LoadConfig()
	if cfg.MaxConns != 25 {
		t.Errorf("MaxConns = %d, want 25", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns = %d, want 2", cfg.MinConns)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}