// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver (no cgo)
)

// DB wraps the SQLite database connection
type DB struct {
	conn *sql.DB
	path string
}

// New opens or creates the NEXUS SQLite database
// Stored at ~/.nexus/nexus.db
func New(dbPath string) (*DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Connection pool settings
	conn.SetMaxOpenConns(1)  // SQLite is single-writer
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(time.Hour)

	db := &DB{conn: conn, path: dbPath}

	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	return db, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.conn.Close()
}

// migrate runs the database schema migrations
func (db *DB) migrate() error {
	schema := `
	-- Requests table: every proxied request
	CREATE TABLE IF NOT EXISTS requests (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		request_id    TEXT,
		model_asked   TEXT NOT NULL,    -- What Claude Code asked for
		model_used    TEXT NOT NULL,    -- What provider actually used
		provider      TEXT NOT NULL,    -- Provider name
		complexity    TEXT NOT NULL,    -- simple|standard|complex|critical
		input_tokens  INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_tokens  INTEGER DEFAULT 0,
		cache_write_tokens INTEGER DEFAULT 0,
		cost_usd      REAL DEFAULT 0,
		cache_saved_usd REAL DEFAULT 0,
		latency_ms    INTEGER DEFAULT 0,
		status        INTEGER DEFAULT 200,
		error         TEXT,
		stream        BOOLEAN DEFAULT FALSE
	);

	-- Indexes for common queries
	CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at);
	CREATE INDEX IF NOT EXISTS idx_requests_provider ON requests(provider);
	CREATE INDEX IF NOT EXISTS idx_requests_model_asked ON requests(model_asked);

	-- Daily stats (materialized for performance)
	CREATE TABLE IF NOT EXISTS daily_stats (
		date          TEXT PRIMARY KEY,
		total_requests INTEGER DEFAULT 0,
		total_tokens   INTEGER DEFAULT 0,
		total_cost_usd REAL DEFAULT 0,
		requests_by_provider TEXT,  -- JSON
		requests_by_complexity TEXT -- JSON
	);

	-- Provider config cache
	CREATE TABLE IF NOT EXISTS providers (
		name        TEXT PRIMARY KEY,
		base_url    TEXT NOT NULL,
		tier        TEXT NOT NULL,
		is_healthy  BOOLEAN DEFAULT TRUE,
		last_check  DATETIME,
		added_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	-- Usage events: one immutable row per completed upstream request attempt
	-- (failover steps, key rotations, and probes each get their own row).
	-- Token columns are NULL when the provider did not report them — 0 is a
	-- genuine reported zero, never a placeholder. Append-only: nothing ever
	-- updates or deletes rows here.
	CREATE TABLE IF NOT EXISTS usage_events (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		provider       TEXT NOT NULL,
		model_used     TEXT NOT NULL,
		model_asked    TEXT NOT NULL DEFAULT '',
		request_id     TEXT,
		key_index      INTEGER,               -- 0-based key-pool index; NULL = no pool
		attempt        INTEGER,               -- 1-based position in the provider chain; 0 = probe
		status         INTEGER NOT NULL,      -- upstream HTTP status; 0 = transport error
		success        BOOLEAN NOT NULL DEFAULT 0,
		stream         BOOLEAN NOT NULL DEFAULT 0,
		in_tokens      INTEGER,               -- fresh input (excludes cached)
		out_tokens     INTEGER,
		cache_read_tokens    INTEGER,
		cache_write_tokens   INTEGER,
		reasoning_tokens     INTEGER,         -- subset of out_tokens (no double count)
		usage_partial  BOOLEAN NOT NULL DEFAULT 0, -- stream aborted / usage only partially known
		duration_ms    INTEGER NOT NULL DEFAULT 0,
		rate_limited   BOOLEAN NOT NULL DEFAULT 0,
		retry_after    REAL,                  -- seconds, from 429 Retry-After
		retry_reset_at DATETIME,              -- absolute reset timestamp from 429
		quota_dimension  TEXT,                -- e.g. "5h"/"7d", transcribed from provider headers
		quota_utilization REAL,               -- 0..1 fraction, exactly as reported
		quota_reset_at   DATETIME,            -- provider-reported quota reset epoch
		quota_meta     TEXT,                 -- JSON: raw captured rate-limit/quota headers
		error          TEXT,
		probe          BOOLEAN NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_usage_events_provider_created ON usage_events(provider, created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events(created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_events_model_used ON usage_events(model_used);
	`

	if _, err := db.conn.Exec(schema); err != nil {
		return err
	}

	// Idempotent column additions for databases created before these columns
	// existed. SQLite has no "ADD COLUMN IF NOT EXISTS", so we run each and
	// ignore the "duplicate column name" error on already-migrated DBs.
	for _, stmt := range []string{
		`ALTER TABLE requests ADD COLUMN cache_read_tokens INTEGER DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN cache_write_tokens INTEGER DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN cache_saved_usd REAL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN prompt TEXT`,   // captured only when --inspect is on
		`ALTER TABLE requests ADD COLUMN response TEXT`, // captured only when --inspect is on
		`ALTER TABLE requests ADD COLUMN user TEXT`,         // team attribution (from X-Nexus-User / nexus-<name> key)
		`ALTER TABLE requests ADD COLUMN redacted INTEGER DEFAULT 0`, // secrets/PII masked by the privacy firewall
	} {
		_, _ = db.conn.Exec(stmt)
	}
	return nil
}

// DefaultDBPath returns the default database path
func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nexus", "nexus.db")
}
