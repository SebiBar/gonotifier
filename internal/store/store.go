// Package store keeps all persistent data in one SQLite database:
// users and their sessions, the events, and the log of reminders already sent.
// Events and sent reminders belong to a user (owner); every query is scoped by owner.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// Timestamps are stored as fixed-width UTC strings so they sort and compare lexically.
const dbTimeFormat = "2006-01-02T15:04:05Z"

// migrations run in order; PRAGMA user_version records how many have been applied.
// Append new ones — never edit or reorder existing entries.
var migrations = []string{
	// 1: users (logins come from ntfy), their sessions and events, and the log of sent reminders
	`CREATE TABLE users (
		username   TEXT PRIMARY KEY,           -- ntfy username
		ntfy_token TEXT NOT NULL,              -- the user's ntfy token, used to send their reminders
		feed_token TEXT NOT NULL UNIQUE,       -- secret part of the calendar feed URL
		created_at TEXT NOT NULL
	);
	CREATE TABLE sessions (
		id         TEXT PRIMARY KEY,           -- sha256 of the session cookie
		username   TEXT NOT NULL,
		expires_at TEXT NOT NULL
	);
	CREATE TABLE events (
		id          TEXT PRIMARY KEY,
		owner       TEXT NOT NULL,              -- username
		name        TEXT NOT NULL,
		date        TEXT NOT NULL,              -- YYYY-MM-DD or YYYY-MM-DDTHH:MM
		repeat      TEXT NOT NULL DEFAULT '',   -- '', daily, weekly, monthly, yearly
		every       INTEGER NOT NULL DEFAULT 0,
		until       TEXT NOT NULL DEFAULT '',
		reminders   TEXT NOT NULL,              -- JSON array, e.g. ["1d","30m"]
		auto_remove INTEGER,                    -- NULL = default (true)
		notify_time TEXT NOT NULL DEFAULT '',
		topic       TEXT NOT NULL DEFAULT '',
		priority    TEXT NOT NULL DEFAULT '',
		tags        TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL
	);
	CREATE TABLE sent (
		id          TEXT PRIMARY KEY,           -- sha256(eventID+offset+occurrenceDate)[:16]
		owner       TEXT NOT NULL,
		event_id    TEXT NOT NULL,
		event_name  TEXT NOT NULL,
		offset      TEXT NOT NULL,
		target_date TEXT NOT NULL,              -- occurrence date, YYYY-MM-DD
		fire_time   TEXT NOT NULL,
		sent_at     TEXT NOT NULL,
		message     TEXT NOT NULL
	);
	CREATE INDEX idx_events_owner ON events(owner);
	CREATE INDEX idx_sent_event ON sent(event_id, target_date);
	CREATE INDEX idx_sent_owner ON sent(owner, sent_at);
	CREATE INDEX idx_sent_at ON sent(sent_at);`,
}

type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes event writes so validation sees a consistent set

	version atomic.Uint64 // bumped on every event change
}

// Open opens (or creates) the database and applies pending migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; avoids SQLITE_BUSY between pooled connections
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var current int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this gonotifier (v%d); upgrade gonotifier", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Version changes whenever events are created, updated or deleted.
func (s *Store) Version() uint64 { return s.version.Load() }
