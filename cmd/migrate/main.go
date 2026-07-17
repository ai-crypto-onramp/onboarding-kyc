// Command migrate applies or rolls back database migrations for the
// onboarding-kyc service. Usage:
//
//	migrate --up      apply all pending migrations
//	migrate --down    roll back the latest migration
//	migrate --status   print applied and pending versions
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ai-crypto-onramp/onboarding-kyc/db"
)

func main() {
	up := flag.Bool("up", false, "apply all pending migrations")
	down := flag.Bool("down", false, "roll back the latest migration")
	status := flag.Bool("status", false, "print migration status")
	flag.Parse()

	if !*up && !*down && !*status {
		fmt.Fprintln(os.Stderr, "usage: migrate [--up|--down|--status]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Pool(ctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	switch {
	case *up:
		n, err := db.MigrateUp(ctx, pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("applied %d migration(s)\n", n)
	case *down:
		n, err := db.MigrateDown(ctx, pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate down: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("rolled back %d migration(s)\n", n)
	case *status:
		migs, err := db.LoadMigrations()
		if err != nil {
			fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
			os.Exit(1)
		}
		if err := db.EnsureSchemaMigrations(ctx, pool); err != nil {
			fmt.Fprintf(os.Stderr, "ensure schema: %v\n", err)
			os.Exit(1)
		}
		for _, m := range migs {
			var applied bool
			_ = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", m.Version).Scan(&applied)
			state := "STARTED"
			if applied {
				state = "applied"
			}
			fmt.Printf("%d %s [%s]\n", m.Version, m.Name, state)
		}
	}
}