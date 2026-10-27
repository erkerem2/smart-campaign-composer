// Command server runs the Smart Campaign Composer HTTP API (internal/api):
//
//	GET  /healthz
//	POST /api/campaigns/preview-segment
//	POST /api/campaigns/compose/stream          (SSE)
//	POST /api/campaigns
//	GET  /api/campaigns/{id}
//	POST /api/campaigns/{id}/dispatch
//	GET  /api/campaigns/{id}/dispatch
//	GET  /api/campaigns/{id}/dispatch/stream    (SSE)
//
// Configuration comes from the environment, optionally from a .env file in
// the working directory or its parent (the repository root when run from
// backend/): HTTP_ADDR (:8080), DB_PATH (data/campaign.db), PROVIDER_URL
// (http://localhost:9090, see cmd/mock-provider), CORS_ALLOWED_ORIGINS,
// ANTHROPIC_API_KEY (unset = mock LLM mode), ANTHROPIC_MODEL.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"smart-campaign-composer/backend/internal/api"
	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/config"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/httpserver"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/repository"
	"smart-campaign-composer/backend/internal/segment"
)

const version = "0.5.0"

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	if loaded, err := config.LoadDotEnv(".env", filepath.Join("..", ".env")); err != nil {
		return fmt.Errorf("load .env: %w", err)
	} else if loaded != "" {
		slog.Info("loaded environment file", "path", loaded)
	}
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if dir := filepath.Dir(cfg.DBPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db directory: %w", err)
		}
	}
	db, err := repository.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	// Migrate also (re)creates the v_users_segment view the segment engine
	// reads (schema version 3).
	if err := repository.Migrate(ctx, db); err != nil {
		return err
	}
	users := repository.NewUserRepository(db)
	if n, err := users.Count(ctx); err != nil {
		return err
	} else if n == 0 {
		slog.Warn("no users in the database; run `make seed` first", "db", cfg.DBPath)
	}

	segmentLLM, messageLLM, err := llmClients(cfg)
	if err != nil {
		return err
	}
	campaigns := repository.NewCampaignRepository(db)
	providerClient := provider.NewClient(cfg.ProviderURL, nil)
	dispatcher, err := dispatch.New(
		repository.NewDispatchRepository(db),
		campaigns,
		providerClient,
		dispatch.NewComposerRenderer(nil),
		dispatch.DefaultConfig(),
	)
	if err != nil {
		return err
	}

	corsOrigins := api.DefaultCORSOrigins()
	if cfg.CORSOrigins != "" {
		corsOrigins = api.ParseOrigins(cfg.CORSOrigins)
	}
	server, err := api.New(api.Config{
		Segments:   segment.NewEngine(segmentLLM, db, segment.DefaultRegistry, segment.SQLite{}),
		Composer:   composer.NewComposer(messageLLM, nil),
		Campaigns:  campaigns,
		Users:      users,
		Dispatcher: dispatcher,
		Health: []api.HealthCheck{
			{Name: "database", Critical: true, Check: func(ctx context.Context) error { return pingDB(ctx, db) }},
			{Name: "provider", Check: providerClient.Ping},
		},
		Metrics: repository.NewMetricsRepository(db),
		// Dispatch runs live as long as the server, not the request.
		BaseContext: ctx,
		CORS:        api.CORSConfig{AllowedOrigins: corsOrigins},
		LLMMode:     cfg.LLMMode(),
		Version:     version,
	})
	if err != nil {
		return err
	}

	// Timeout policy (SSE-safe, no WriteTimeout): see internal/httpserver.
	srv := httpserver.New(cfg.HTTPAddr, server.Handler())

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr, "db", cfg.DBPath, "provider", cfg.ProviderURL,
			"llm", cfg.LLMMode(), "cors", corsOrigins)
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
	// ctx is cancelled: active runs stop, their pending deliveries stay
	// claimed and the campaign stays 'sending' (POST .../dispatch resumes).
	dispatcher.Wait()
	return err
}

// llmClients returns the Anthropic clients when ANTHROPIC_API_KEY is set,
// otherwise deterministic mocks that answer every brief (the reference rule
// and content), so the whole flow can be demoed without a key.
func llmClients(cfg config.Config) (segment.LLMClient, composer.MessageLLMClient, error) {
	if cfg.AnthropicKey != "" {
		s, err := segment.NewAnthropicClientFromEnv()
		if err != nil {
			return nil, nil, err
		}
		m, err := composer.NewAnthropicMessageClientFromEnv()
		if err != nil {
			return nil, nil, err
		}
		return s, m, nil
	}
	slog.Warn("ANTHROPIC_API_KEY is not set: using the mock LLM (every brief gets the reference rule and content)")
	s := segment.NewMockLLMClient().
		On(composer.ReferenceBrief, segment.ReferenceRule).
		Default(segment.ReferenceRule)
	m := composer.NewMockMessageLLMClient().Default(composer.ReferenceMessageJSON)
	m.ChunkDelay = cfg.MockStreamDelay
	return s, m, nil
}

func pingDB(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// Also proves the segment view compiles against the current schema.
	var n int
	return db.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM "+repository.SegmentView+" LIMIT 1)").Scan(&n)
}
