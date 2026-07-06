// Package main is the migrate CLI. It applies or rolls back embedded SQL
// migrations against the database referenced by DB_URL.
//
// Usage:
//
//	DB_URL=postgres://... go run ./cmd/migrate up
//	DB_URL=postgres://... go run ./cmd/migrate down        # roll back everything
//	DB_URL=postgres://... go run ./cmd/migrate down 3      # roll back down to (excluding) version 3
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ai-crypto-onramp/onboarding-kyc/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "DB_URL is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse DB_URL: %v\n", err)
		os.Exit(1)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ping db: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "up":
		if err := store.MigrateUp(ctx, pool); err != nil {
			fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("migrations applied")
	case "down":
		target := 0
		if len(os.Args) >= 3 {
			n, err := strconv.Atoi(os.Args[2])
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid target version %q: %v\n", os.Args[2], err)
				os.Exit(2)
			}
			target = n
		}
		if err := store.MigrateDown(ctx, pool, target); err != nil {
			fmt.Fprintf(os.Stderr, "migrate down: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("migrations rolled back to version %d\n", target)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: migrate [up|down [target-version]]")
}