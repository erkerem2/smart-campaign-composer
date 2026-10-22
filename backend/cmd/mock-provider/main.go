// Command mock-provider runs the mock SMS / e-mail provider as a standalone
// HTTP service (see internal/provider). It sends nothing to anyone.
//
// Environment:
//
//	MOCK_PROVIDER_ADDR        listen address            (default :9090)
//	MOCK_RATE_LIMITED_RATIO   share of 429 responses    (default 0.10)
//	MOCK_SERVER_ERROR_RATIO   share of 500 responses    (default 0.05)
//	MOCK_MIN_LATENCY          e.g. 10ms                 (default 10ms)
//	MOCK_MAX_LATENCY          e.g. 30ms                 (default 30ms)
//	MOCK_RETRY_AFTER          Retry-After on 429        (default 1s)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"smart-campaign-composer/backend/internal/provider"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mock provider stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := provider.DefaultConfig()
	var err error
	if cfg.RateLimitedRatio, err = envFloat("MOCK_RATE_LIMITED_RATIO", cfg.RateLimitedRatio); err != nil {
		return err
	}
	if cfg.ServerErrorRatio, err = envFloat("MOCK_SERVER_ERROR_RATIO", cfg.ServerErrorRatio); err != nil {
		return err
	}
	if cfg.MinLatency, err = envDuration("MOCK_MIN_LATENCY", cfg.MinLatency); err != nil {
		return err
	}
	if cfg.MaxLatency, err = envDuration("MOCK_MAX_LATENCY", cfg.MaxLatency); err != nil {
		return err
	}
	if cfg.RetryAfter, err = envDuration("MOCK_RETRY_AFTER", cfg.RetryAfter); err != nil {
		return err
	}
	mock, err := provider.NewMockServer(cfg)
	if err != nil {
		return err
	}

	addr := os.Getenv("MOCK_PROVIDER_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	srv := &http.Server{Addr: addr, Handler: mock.Handler(), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		slog.Info("mock provider listening", "addr", addr,
			"rate_limited", cfg.RateLimitedRatio, "server_errors", cfg.ServerErrorRatio,
			"latency", fmt.Sprintf("%v-%v", cfg.MinLatency, cfg.MaxLatency))
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
