package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
)

// Record is one sent reminder.
type Record struct {
	ID         string
	Owner      string
	EventID    string
	EventName  string
	Offset     string
	TargetDate string // occurrence date
	FireTime   time.Time
	SentAt     time.Time
	Message    string
}

// ReminderKey identifies one reminder of one occurrence: sha256(eventID|offset|occurrenceDate)[:16].
// Each occurrence of a repeating event gets fresh keys, so it is reminded again.
func ReminderKey(eventID, offset, occurrenceDate string) string {
	h := sha256.Sum256([]byte(eventID + "|" + offset + "|" + occurrenceDate))
	return hex.EncodeToString(h[:])[:16]
}

// IsSent reports whether the reminder with this key was already sent.
func (s *Store) IsSent(key string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM sent WHERE id = ?`, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RecordSent logs a sent reminder.
func (s *Store) RecordSent(r Record) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO sent (id, owner, event_id, event_name, offset, target_date, fire_time, sent_at, message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Owner, r.EventID, r.EventName, r.Offset, r.TargetDate,
		r.FireTime.UTC().Format(dbTimeFormat), clock.Now().UTC().Format(dbTimeFormat), r.Message)
	return err
}

// RecentHistory returns the user's most recent sent reminders, newest first.
func (s *Store) RecentHistory(owner string, limit int) ([]Record, error) {
	rows, err := s.db.Query(
		`SELECT id, owner, event_id, event_name, offset, target_date, fire_time, sent_at, message
		 FROM sent WHERE owner = ? ORDER BY sent_at DESC, rowid DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var fire, sent string
		if err := rows.Scan(&r.ID, &r.Owner, &r.EventID, &r.EventName, &r.Offset, &r.TargetDate, &fire, &sent, &r.Message); err != nil {
			return nil, err
		}
		r.FireTime, _ = time.Parse(dbTimeFormat, fire)
		r.SentAt, _ = time.Parse(dbTimeFormat, sent)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Prune removes sent records older than the given duration.
func (s *Store) Prune(olderThan time.Duration) error {
	cutoff := clock.Now().Add(-olderThan).UTC().Format(dbTimeFormat)
	_, err := s.db.Exec(`DELETE FROM sent WHERE sent_at < ?`, cutoff)
	return err
}
