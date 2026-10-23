// Package repository implements persistence on top of SQLite.
package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/domain"

	// Pure-Go SQLite driver: no CGO, no gcc, works the same on Windows,
	// macOS, Linux and in a scratch Docker image. Registers as "sqlite".
	_ "modernc.org/sqlite"
)

// DriverName is the database/sql driver used for SQLite.
const DriverName = "sqlite"

// ErrNotFound is returned when a requested row does not exist. It is the
// domain sentinel, so use cases can test for it without importing this
// package.
var ErrNotFound = domain.ErrNotFound

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

// SchemaVersion is stored in SQLite's PRAGMA user_version.
//
//	1  initial schema (ADR 003)
//	2  dispatch_logs gains the 'pending' claim state, last_error and
//	   created_at/updated_at (ADR 007)
//	3  v_users_segment view with the derived segment attributes, and
//	   campaigns.segment_rule for the approved rule (ADR 008)
const SchemaVersion = 3

// SegmentView is the view the segment engine queries (see schema.sql). It
// must match segment.DefaultRegistry.Table; a repository test enforces it.
const SegmentView = "v_users_segment"

// Migrate brings the database to SchemaVersion. It is idempotent and runs in
// one transaction, so a failed upgrade leaves the previous schema intact.
//
// schema.sql always describes the latest schema with CREATE ... IF NOT
// EXISTS, so a fresh database is created at the latest version directly.
// Older databases are upgraded by the steps below before schema.sql runs.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("migrate: read user_version: %w", err)
	}

	legacyDispatchLogs := false
	if version < 2 {
		if legacyDispatchLogs, err = detachLegacyDispatchLogs(ctx, tx); err != nil {
			return fmt.Errorf("migrate v2: %w", err)
		}
	}
	if version < 3 {
		if err := addCampaignSegmentRule(ctx, tx); err != nil {
			return fmt.Errorf("migrate v3: %w", err)
		}
	}
	// schema.sql also drops and recreates v_users_segment, so the view
	// always matches the current definition.
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if legacyDispatchLogs {
		if err := copyLegacyDispatchLogs(ctx, tx); err != nil {
			return fmt.Errorf("migrate v2: %w", err)
		}
	}
	if version < SchemaVersion {
		// PRAGMA does not accept bind parameters; SchemaVersion is a Go constant.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
			return fmt.Errorf("migrate: write user_version: %w", err)
		}
	}
	return tx.Commit()
}

// detachLegacyDispatchLogs renames a version-1 dispatch_logs table (whose
// CHECK constraint does not allow 'pending') out of the way. SQLite cannot
// alter a CHECK constraint in place, so the table is rebuilt: schema.sql
// then creates the new table and copyLegacyDispatchLogs moves the rows over.
func detachLegacyDispatchLogs(ctx context.Context, tx *sql.Tx) (bool, error) {
	var ddl string
	err := tx.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'dispatch_logs'").Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // fresh database
	}
	if err != nil {
		return false, err
	}
	if strings.Contains(ddl, "'pending'") {
		return false, nil // already the v2 table (created by a newer schema.sql)
	}
	for _, stmt := range []string{
		"ALTER TABLE dispatch_logs RENAME TO dispatch_logs_v1",
		// The index keeps its name after the rename; drop it so schema.sql
		// recreates it on the new table.
		"DROP INDEX IF EXISTS idx_dispatch_logs_campaign_status",
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return true, nil
}

// addCampaignSegmentRule adds campaigns.segment_rule to a pre-v3 database.
// A fresh database gets the column from schema.sql directly. Adding a
// nullable column is an in-place ALTER in SQLite (no table rebuild).
func addCampaignSegmentRule(ctx context.Context, tx *sql.Tx) error {
	var tables, columns int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'campaigns'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return nil // fresh database
	}
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM pragma_table_info('campaigns') WHERE name = 'segment_rule'").Scan(&columns); err != nil {
		return err
	}
	if columns > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `ALTER TABLE campaigns ADD COLUMN segment_rule TEXT
		CHECK (segment_rule IS NULL OR json_valid(segment_rule))`)
	return err
}

func copyLegacyDispatchLogs(ctx context.Context, tx *sql.Tx) error {
	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dispatch_logs (id, campaign_id, user_id, channel, status, provider_message_id,
			retry_count, sent_at, created_at, updated_at)
		SELECT id, campaign_id, user_id, channel, status, provider_message_id, retry_count,
			CASE WHEN status = 'sent' THEN COALESCE(sent_at, ?1) ELSE sent_at END,
			COALESCE(sent_at, ?1), COALESCE(sent_at, ?1)
		FROM dispatch_logs_v1`, now); err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE dispatch_logs_v1"); err != nil {
		return fmt.Errorf("drop legacy table: %w", err)
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
