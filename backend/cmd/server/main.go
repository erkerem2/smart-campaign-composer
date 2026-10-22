// Command server runs the Smart Campaign Composer HTTP API.
//
// It opens and migrates the database, wires the dispatch engine to the mock
// provider and exposes health and live dispatch-tracking endpoints:
//
//	GET /healthz                                   database + provider checks
//	GET /api/campaigns/{id}/dispatch               dispatch stats snapshot (JSON)
//	GET /api/campaigns/{id}/dispatch/stream        live stats (Server-Sent Events)
//
// Environment: HTTP_ADDR (:8080), DB_PATH (data/campaign.db),
// PROVIDER_URL (http://localhost:9090, see cmd/mock-provider).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/repository"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := envOr("HTTP_ADDR", ":8080")
	dbPath := envOr("DB_PATH", filepath.Join("data", "campaign.db"))
	providerURL := envOr("PROVIDER_URL", "http://localhost:9090")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db directory: %w", err)
		}
	}
	db, err := repository.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := repository.Migrate(ctx, db); err != nil {
		return err
	}

	// Runs are started with the server's lifetime context (the start/resume
	// endpoints arrive with the campaign API) and drained on shutdown;
	// interrupted runs stay 'sending' and can be resumed.
	dispatcher, err := dispatch.New(
		repository.NewDispatchRepository(db),
		repository.NewCampaignRepository(db),
		provider.NewClient(providerURL, nil),
		dispatch.NewComposerRenderer(nil),
		dispatch.DefaultConfig(),
	)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(db, providerURL))
	mux.HandleFunc("GET /api/campaigns/{id}/dispatch", dispatcher.StatsHandler())
	mux.HandleFunc("GET /api/campaigns/{id}/dispatch/stream", dispatcher.StreamHandler(500*time.Millisecond))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr, "db", dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	dispatcher.Wait()
	return err
}

// healthHandler reports 200 when the database answers, 503 otherwise. The
// provider is reported but does not make the API unhealthy: dispatch retries
// through provider outages and the rest of the API keeps working.
func healthHandler(db *sql.DB, providerURL string) http.HandlerFunc {
	client := &http.Client{Timeout: 2 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, code := "ok", http.StatusOK
		dbStatus := "ok"
		if err := db.PingContext(ctx); err != nil {
			status, code, dbStatus = "degraded", http.StatusServiceUnavailable, err.Error()
		}
		providerStatus := "ok"
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(providerURL, "/")+"/healthz", nil)
		if resp, err := client.Do(req); err != nil {
			providerStatus = "unreachable"
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				providerStatus = resp.Status
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": status,
			"checks": map[string]string{"database": dbStatus, "provider": providerStatus},
		})
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
