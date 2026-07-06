// Package main is the onboarding-kyc service entrypoint. It wires the store
// (Postgres pool) into the HTTP server, runs migrations on startup, and serves
// /healthz reflecting the database health.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ai-crypto-onramp/onboarding-kyc/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := store.LoadConfig()

	var health *store.HealthChecker
	if cfg.DBURL != "" {
		pool, err := store.Open(ctx, cfg)
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
		defer pool.Close()
		health = store.NewHealthChecker(pool)
	} else {
		log.Println("DB_URL not set; running without a database")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           newMux(health),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("onboarding-kyc listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func newMux(health *store.HealthChecker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(health))
	return mux
}

func healthzHandler(h *store.HealthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.Check(ctx); err != nil {
			http.Error(w, "unhealthy: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}