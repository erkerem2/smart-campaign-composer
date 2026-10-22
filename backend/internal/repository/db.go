// Package repository implements persistence on top of SQLite.
package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"errors"
	"fmt"
	"time"

	// Pure-Go SQLite driver: no CGO, no gcc, works the same on Windows,
	// macOS, Linux and in a scratch Docker image. Registers as "sqlite".
	_ "modernc.org/sqlite"
)

// DriverName is the database/sql driver used for SQLite.
const DriverName = "sqlite"

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

//go:embed schema.sql
var schemaSQL string

// connectionPragmas run on every new connection. foreign_keys and
// busy_timeout are per-connection settings in SQLite, so setting them once
// after Open would silently miss connections the pool opens later.
var connectionPragmas = []string{
	"PRAGMA foreign_keys = ON",
	"PRAGMA busy_timeout = 5000",
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = NORMAL",
}

// Open opens (creating if needed) the SQLite database at path and verifies
// the connection. Callers own the returned *sql.DB and must Close it.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	connector, err := newPragmaConnector(DriverName, path, connectionPragmas)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	return db, nil
}

// Migrate creates all tables and indexes. It is idempotent.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// ResetData deletes every row from every table (children first) in a single
// transaction. Intended for the seed command's -reset flag and tests.
func ResetData(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"dispatch_logs", "campaign_messages", "campaigns", "users"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("reset %s: %w", table, err)
		}
	}
	// Restart AUTOINCREMENT counters so a reseed yields ids 1..N again.
	// sqlite_sequence only exists once an AUTOINCREMENT row was inserted.
	var hasSequence int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'").Scan(&hasSequence); err != nil {
		return err
	}
	if hasSequence > 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sqlite_sequence"); err != nil {
			return fmt.Errorf("reset sqlite_sequence: %w", err)
		}
	}
	return tx.Commit()
}

// pragmaConnector wraps a driver so that a fixed list of statements runs on
// every physical connection the pool opens. It only relies on
// database/sql/driver interfaces, so it is independent of the SQLite driver.
type pragmaConnector struct {
	drv     driver.Driver
	dsn     string
	pragmas []string
}

func newPragmaConnector(driverName, dsn string, pragmas []string) (*pragmaConnector, error) {
	// sql.Open does not connect; it is the portable way to look up a
	// registered driver by name.
	probe, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	drv := probe.Driver()
	_ = probe.Close()
	return &pragmaConnector{drv: drv, dsn: dsn, pragmas: pragmas}, nil
}

func (c *pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("sqlite driver does not implement driver.ExecerContext")
	}
	for _, p := range c.pragmas {
		if _, err := execer.ExecContext(ctx, p, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return conn, nil
}

func (c *pragmaConnector) Driver() driver.Driver { return c.drv }
