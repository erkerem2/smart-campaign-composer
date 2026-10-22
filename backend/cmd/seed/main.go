// Command seed fills the database with realistic fake users.
//
//	go run ./cmd/seed                 # seed once; no-op if users already exist
//	go run ./cmd/seed -reset          # wipe all tables and reseed
//	go run ./cmd/seed -count 5000 -seed 7
//
// Dates are generated relative to the moment the command runs, so "last 30
// days" cohorts stay meaningful. Re-run with -reset if the data gets stale.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"smart-campaign-composer/backend/internal/repository"
	"smart-campaign-composer/backend/internal/seed"
)

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath  = flag.String("db", envOr("DB_PATH", filepath.Join("data", "campaign.db")), "SQLite database path (env DB_PATH)")
		count   = flag.Int("count", seed.DefaultCount, "number of users to create")
		rngSeed = flag.Uint64("seed", seed.DefaultSeed, "random seed for reproducible data")
		reset   = flag.Bool("reset", false, "delete all existing data before seeding")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if dir := filepath.Dir(*dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db directory: %w", err)
		}
	}

	db, err := repository.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := repository.Migrate(ctx, db); err != nil {
		return err
	}

	users := repository.NewUserRepository(db)
	existing, err := users.Count(ctx)
	if err != nil {
		return err
	}
	switch {
	case *reset:
		slog.Info("resetting database", "path", *dbPath, "existing_users", existing)
		if err := repository.ResetData(ctx, db); err != nil {
			return err
		}
	case existing > 0:
		// Idempotent: safe to run on every `docker compose up`.
		slog.Info("users already present, skipping (use -reset to reseed)", "path", *dbPath, "users", existing)
		return nil
	}

	now := time.Now().UTC()
	generated, err := seed.Generate(seed.Config{Count: *count, Seed: *rngSeed, Now: now})
	if err != nil {
		return err
	}

	start := time.Now()
	if err := users.InsertBatch(ctx, generated); err != nil {
		return err
	}
	slog.Info("seed complete", "path", *dbPath, "users", len(generated), "seed", *rngSeed,
		"took", time.Since(start).Round(time.Millisecond))

	fmt.Print(seed.Summarize(generated, now))
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
