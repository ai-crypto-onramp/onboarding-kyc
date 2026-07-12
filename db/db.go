// Package db provides PostgreSQL connection pooling and an embedded SQL
// migration runner for the onboarding-kyc service.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Config holds database connection configuration derived from DB_URL and
// optional env overrides.
type Config struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// DefaultConfig returns a Config populated from DB_URL and sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DSN:             os.Getenv("DB_URL"),
		MaxConns:        envInt("DB_MAX_CONNS", 20),
		MinConns:        envInt("DB_MIN_CONNS", 2),
		MaxConnLifetime: envDuration("DB_MAX_CONN_LIFETIME", 30*time.Minute),
		MaxConnIdleTime: envDuration("DB_MAX_CONN_IDLE_TIME", 5*time.Minute),
		ConnectTimeout:  envDuration("DB_CONNECT_TIMEOUT", 5*time.Second),
	}
}

// Pool opens a pgx connection pool configured from cfg.
func Pool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if cfg.DSN == "" {
		return nil, fmt.Errorf("DB_URL is required")
	}
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DB_URL: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

// Migration represents a single SQL migration file pair.
type Migration struct {
	Version int
	Name    string
	UpSQL   string
	DownSQL string
}

// LoadMigrations reads embedded migration SQL files and returns them sorted
// by version.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	upFiles := map[int]string{}
	downFiles := map[int]string{}
	names := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) != 2 {
			continue
		}
		ver, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		data, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if strings.HasSuffix(e.Name(), ".up.sql") {
			upFiles[ver] = string(data)
			names[ver] = strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".up.sql"), "")
		} else if strings.HasSuffix(e.Name(), ".down.sql") {
			downFiles[ver] = string(data)
		}
	}
	var versions []int
	for v := range upFiles {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	out := make([]Migration, 0, len(versions))
	for _, v := range versions {
		out = append(out, Migration{
			Version: v,
			Name:    names[v],
			UpSQL:   upFiles[v],
			DownSQL: downFiles[v],
		})
	}
	return out, nil
}

// EnsureSchemaMigrations creates the schema_migrations tracking table if it
// does not exist.
func EnsureSchemaMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`)
	return err
}

// MigrateUp applies all pending migrations in version order.
func MigrateUp(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if err := EnsureSchemaMigrations(ctx, pool); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}
	migs, err := LoadMigrations()
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, m := range migs {
		var exists bool
		err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", m.Version).Scan(&exists)
		if err != nil {
			return applied, fmt.Errorf("check version %d: %w", m.Version, err)
		}
		if exists {
			continue
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return applied, fmt.Errorf("begin tx for v%d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, m.UpSQL); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("apply v%d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", m.Version); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("record v%d: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("commit v%d: %w", m.Version, err)
		}
		applied++
	}
	return applied, nil
}

// MigrateDown rolls back the latest applied migration.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if err := EnsureSchemaMigrations(ctx, pool); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}
	migs, err := LoadMigrations()
	if err != nil {
		return 0, err
	}
	var latest int
	err = pool.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&latest)
	if err != nil {
		return 0, fmt.Errorf("find latest: %w", err)
	}
	if latest == 0 {
		return 0, nil
	}
	var downSQL string
	for _, m := range migs {
		if m.Version == latest {
			downSQL = m.DownSQL
			break
		}
	}
	if downSQL == "" {
		return 0, fmt.Errorf("no down migration for version %d", latest)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.Exec(ctx, downSQL); err != nil {
		_ = tx.Rollback(ctx)
		return 0, fmt.Errorf("apply down v%d: %w", latest, err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version=$1", latest); err != nil {
		_ = tx.Rollback(ctx)
		return 0, fmt.Errorf("delete v%d: %w", latest, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit down: %w", err)
	}
	return 1, nil
}

func envInt(key string, def int32) int32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			return int32(n)
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}