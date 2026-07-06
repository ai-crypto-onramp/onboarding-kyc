// Package store wires the PostgreSQL persistence layer for the onboarding-kyc
// service: a pgxpool bootstrap from DB_URL, an embedded SQL migration runner,
// and a HealthChecker for the /healthz handler.
package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds the store configuration read from the environment.
type Config struct {
	DBURL            string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
}

// LoadConfig reads store configuration from environment variables using the
// defaults documented in README.md.
func LoadConfig() Config {
	return Config{
		DBURL:           os.Getenv("DB_URL"),
		MaxConns:        int32(envInt("DB_MAX_CONNS", 25)),
		MinConns:        int32(envInt("DB_MIN_CONNS", 2)),
		MaxConnLifetime: envDuration("DB_CONN_MAX_LIFETIME", 300*time.Second),
		MaxConnIdleTime: envDuration("DB_CONN_MAX_IDLE_TIME", 60*time.Second),
	}
}

// Open opens a pooled Postgres connection, pings it, and applies all pending
// migrations. The returned pool must be closed by the caller.
func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.DBURL == "" {
		return nil, fmt.Errorf("DB_URL is required")
	}
	pcfg, err := pgxpool.ParseConfig(cfg.DBURL)
	if err != nil {
		return nil, fmt.Errorf("parse DB_URL: %w", err)
	}
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := MigrateUp(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return pool, nil
}

// HealthChecker runs a liveness probe against the pool for the /healthz handler.
type HealthChecker struct {
	pool *pgxpool.Pool
}

// NewHealthChecker returns a HealthChecker for the given pool (may be nil).
func NewHealthChecker(pool *pgxpool.Pool) *HealthChecker {
	return &HealthChecker{pool: pool}
}

// Check returns nil if the database is reachable, otherwise an error
// describing the failure.
func (h *HealthChecker) Check(ctx context.Context) error {
	if h.pool == nil {
		return nil
	}
	if err := h.pool.Ping(ctx); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	return nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}