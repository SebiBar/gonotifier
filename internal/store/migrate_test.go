package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
)

// releasedMigrations are the sha256 of every migration that has been released. A database that
// already ran a migration never runs it again, so editing one breaks every existing database:
// change the schema by appending a migration, then add its hash here.
var releasedMigrations = []string{
	"301e149ef0120fe89c3a9fe8344b4d5e4b04ef3873e1aa51f3bc2423672ab1a6", // 1: users, sessions, events, sent
}

func TestMigrationsAreAppendOnly(t *testing.T) {
	if len(migrations) < len(releasedMigrations) {
		t.Fatalf("a released migration was removed: have %d, released %d", len(migrations), len(releasedMigrations))
	}
	for i, m := range migrations {
		sum := sha256.Sum256([]byte(m))
		got := hex.EncodeToString(sum[:])
		if i >= len(releasedMigrations) {
			t.Errorf("migration %d is new: once it's final, add its hash to releasedMigrations: %s", i+1, got)
		} else if got != releasedMigrations[i] {
			t.Errorf("released migration %d was edited (hash %s). Undo that and append a new migration instead", i+1, got)
		}
	}
}

func TestOpen_RejectsDatabaseFromAnotherVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gn.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// What happened on 2026-10-12: a database whose columns differ from this version's at the
	// same schema version (here: the column as an older version named it).
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE events RENAME COLUMN day_start TO notify_time`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if st, err := Open(path); err == nil {
		st.Close()
		t.Fatal("opened a database that doesn't match this version")
	}

	// A database from a newer version (more migrations) is refused too.
	db, _ = sql.Open("sqlite", "file:"+path)
	db.Exec(`ALTER TABLE events RENAME COLUMN notify_time TO day_start`)
	db.Exec(`PRAGMA user_version = 99`)
	db.Close()
	if st, err := Open(path); err == nil {
		st.Close()
		t.Fatal("opened a database from a newer version")
	}
}
