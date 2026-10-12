// Package store keeps all persistent data in one SQLite database:
// users and their sessions, the events, and the log of reminders already sent.
// Events and sent reminders belong to a user (owner); every query is scoped by owner.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// Timestamps are stored as fixed-width UTC strings so they sort and compare lexically.
const dbTimeFormat = "2006-01-02T15:04:05Z"

// migrations run in order; PRAGMA user_version records how many have been applied.
// Append new ones — never edit or reorder existing entries: a database that already ran a
// migration never runs it again. TestMigrationsAreAppendOnly enforces this.
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
		day_start TEXT NOT NULL DEFAULT '',
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
	if err := checkSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// checkSchema compares the database's tables with what this version's migrations create. A
// database made or changed by another version (e.g. after rolling back) then fails at startup
// with a clear error, instead of starting and failing on every query.
func checkSchema(db *sql.DB) error {
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer fresh.Close()
	for i, m := range migrations {
		if _, err := fresh.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	want, err := tableColumns(fresh)
	if err != nil {
		return err
	}
	have, err := tableColumns(db)
	if err != nil {
		return err
	}
	for table, cols := range want {
		if !slices.Equal(have[table], cols) {
			return fmt.Errorf("the database doesn't match this version of gonotifier: table %s has columns %v, "+
				"this version expects %v. Run the version that created it, or restore a backup made with this one",
				table, have[table], cols)
		}
	}
	return nil
}

// tableColumns lists each table's columns, in order.
func tableColumns(db *sql.DB) (map[string][]string, error) {
	rows, err := db.Query(`SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name, p.cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string][]string{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return nil, err
		}
		tables[table] = append(tables[table], col)
	}
	return tables, rows.Err()
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
