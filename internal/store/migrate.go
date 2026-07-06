package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration represents a single versioned SQL migration pair.
type Migration struct {
	Version int
	Up      string
	Down    string
}

// LoadMigrations reads embedded migration files and returns them sorted by version.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	byVersion := make(map[int]*Migration)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}

		var version int
		var kind string
		if _, err := fmt.Sscanf(name, "%d_%s", &version, &kind); err != nil {
			return nil, fmt.Errorf("parse migration name %s: %w", name, err)
		}

		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version}
			byVersion[version] = m
		}
		if strings.HasSuffix(name, ".up.sql") {
			m.Up = string(body)
		} else if strings.HasSuffix(name, ".down.sql") {
			m.Down = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("migration %d missing up script", m.Version)
		}
		if m.Down == "" {
			return nil, fmt.Errorf("migration %d missing down script", m.Version)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// MigrateUp applies all pending up-migrations to the pool. It is idempotent: an
// internal schema_migrations table tracks applied versions.
func MigrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	migrations, err := LoadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		var applied int
		err := pool.QueryRow(ctx, `SELECT 1 FROM schema_migrations WHERE version = $1`, m.Version).Scan(&applied)
		if err == nil {
			continue
		}
		if err.Error() != "no rows in result set" {
			return fmt.Errorf("check migration %d: %w", m.Version, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for migration %d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, m.Up); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %d up: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
	}
	return nil
}

// MigrateDown rolls back migrations in reverse version order down to (but not
// including) targetVersion. If targetVersion is 0, all migrations are rolled back.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, targetVersion int) error {
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	migrations, err := LoadMigrations()
	if err != nil {
		return err
	}

	for i := len(migrations) - 1; i >= 0; i-- {
		m := migrations[i]
		if m.Version <= targetVersion {
			break
		}
		var applied int
		err := pool.QueryRow(ctx, `SELECT 1 FROM schema_migrations WHERE version = $1`, m.Version).Scan(&applied)
		if err != nil {
			if err.Error() == "no rows in result set" {
				continue
			}
			return fmt.Errorf("check migration %d: %w", m.Version, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for migration %d down: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, m.Down); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %d down: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.Version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("delete migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d down: %w", m.Version, err)
		}
	}
	return nil
}