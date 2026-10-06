package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/events"
)

// ErrNotFound is returned when the requested event, user or session doesn't exist.
var ErrNotFound = errors.New("not found")

var reID = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const eventColumns = `id, name, date, repeat, every, until, reminders, auto_remove, day_start, topic, priority, tags, owner`

type scanner interface{ Scan(...any) error }

func scanEvent(row scanner) (events.Event, error) {
	var e events.Event
	var reminders string
	var autoRemove sql.NullBool
	if err := row.Scan(&e.ID, &e.Name, &e.Date, &e.Repeat, &e.Every, &e.Until, &reminders, &autoRemove,
		&e.DayStart, &e.Topic, &e.Priority, &e.Tags, &e.Owner); err != nil {
		return e, err
	}
	if err := json.Unmarshal([]byte(reminders), &e.Reminders); err != nil {
		return e, fmt.Errorf("event %s: bad reminders: %w", e.ID, err)
	}
	if autoRemove.Valid {
		v := autoRemove.Bool
		e.AutoRemove = &v
	}
	return e, nil
}

// ListEvents returns one user's events in creation order.
func (s *Store) ListEvents(owner string) ([]events.Event, error) {
	return s.queryEvents(`SELECT `+eventColumns+` FROM events WHERE owner = ? ORDER BY created_at, rowid`, owner)
}

// AllEvents returns every user's events in creation order (for the scheduler).
func (s *Store) AllEvents() ([]events.Event, error) {
	return s.queryEvents(`SELECT ` + eventColumns + ` FROM events ORDER BY created_at, rowid`)
}

func (s *Store) queryEvents(query string, args ...any) ([]events.Event, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []events.Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEvent returns one of the user's events, or ErrNotFound.
func (s *Store) GetEvent(owner, id string) (events.Event, error) {
	e, err := scanEvent(s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE id = ? AND owner = ?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// CreateEvent normalizes, validates and stores a new event for the user, with a fresh ID.
// Validation failures are returned as events.ValidationError.
func (s *Store) CreateEvent(owner string, e events.Event) (events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.ListEvents(owner)
	if err != nil {
		return e, err
	}
	events.Normalize(&e)
	if errs := events.Validate(e, existing, ""); len(errs) > 0 {
		return e, events.ValidationError(errs)
	}
	e.ID, e.Owner = newID(), owner
	if err := upsert(s.db, e); err != nil {
		return e, err
	}
	s.version.Add(1)
	return e, nil
}

// UpdateEvent replaces the user's event with the given ID, keeping the ID.
func (s *Store) UpdateEvent(owner, id string, e events.Event) (events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.GetEvent(owner, id); err != nil {
		return e, err
	}
	existing, err := s.ListEvents(owner)
	if err != nil {
		return e, err
	}
	events.Normalize(&e)
	if errs := events.Validate(e, existing, id); len(errs) > 0 {
		return e, events.ValidationError(errs)
	}
	e.ID, e.Owner = id, owner
	if err := upsert(s.db, e); err != nil {
		return e, err
	}
	s.version.Add(1)
	return e, nil
}

// DeleteEvent removes one of the user's events. Its sent history is kept for the history view.
func (s *Store) DeleteEvent(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM events WHERE id = ? AND owner = ?`, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.version.Add(1)
	return nil
}

// ImportResult summarizes an Import.
type ImportResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Deleted int `json:"deleted"`
}

// Import adds or updates the user's events in one transaction: an event whose id
// matches one of theirs replaces it, anything else is created (keeping a valid given
// id, unless another user's event has it). With replace, their events missing from
// the import are deleted. Nothing is written if any event is invalid.
func (s *Store) Import(owner string, incoming []events.Event, replace bool) (ImportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res ImportResult
	current, err := s.ListEvents(owner)
	if err != nil {
		return res, err
	}
	taken, err := s.otherOwnersIDs(owner)
	if err != nil {
		return res, err
	}
	exists := map[string]bool{}
	for _, e := range current {
		exists[e.ID] = true
	}

	// Validate against the set as it will look after the import.
	var final []events.Event
	if !replace {
		final = append(final, current...)
	}
	index := map[string]int{}
	for i, e := range final {
		index[e.ID] = i
	}
	var errs events.ValidationError
	kept := map[string]bool{}
	for i, e := range incoming {
		events.Normalize(&e)
		label := fmt.Sprintf("Event %d (%s):", i+1, e.Name)
		if e.ID != "" && !reID.MatchString(e.ID) {
			errs = append(errs, label+" id may only contain a-z and 0-9.")
			continue
		}
		if e.ID == "" || taken[e.ID] {
			e.ID = newID()
		}
		e.Owner = owner
		if kept[e.ID] {
			errs = append(errs, label+" duplicate id "+e.ID+".")
			continue
		}
		kept[e.ID] = true
		for _, msg := range events.Validate(e, final, e.ID) {
			errs = append(errs, label+" "+msg)
		}
		if j, ok := index[e.ID]; ok {
			final[j] = e
		} else {
			index[e.ID] = len(final)
			final = append(final, e)
		}
		if exists[e.ID] {
			res.Updated++
		} else {
			res.Created++
		}
	}
	if len(errs) > 0 {
		return ImportResult{}, errs
	}

	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	if replace {
		for _, e := range current {
			if !kept[e.ID] {
				if _, err := tx.Exec(`DELETE FROM events WHERE id = ? AND owner = ?`, e.ID, owner); err != nil {
					return res, err
				}
				res.Deleted++
			}
		}
	}
	for _, e := range final {
		if kept[e.ID] {
			if err := upsert(tx, e); err != nil {
				return res, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	s.version.Add(1)
	return res, nil
}

// otherOwnersIDs returns the IDs of events that belong to other users.
func (s *Store) otherOwnersIDs(owner string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM events WHERE owner != ?`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

// upsert writes e. An existing row is only replaced if it has the same owner.
func upsert(db execer, e events.Event) error {
	reminders, _ := json.Marshal(e.Reminders)
	var autoRemove any
	if e.AutoRemove != nil {
		autoRemove = *e.AutoRemove
	}
	_, err := db.Exec(`INSERT INTO events (`+eventColumns+`, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, date = excluded.date, repeat = excluded.repeat, every = excluded.every,
			until = excluded.until, reminders = excluded.reminders, auto_remove = excluded.auto_remove,
			day_start = excluded.day_start, topic = excluded.topic, priority = excluded.priority,
			tags = excluded.tags
		WHERE events.owner = excluded.owner`,
		e.ID, e.Name, e.Date, e.Repeat, e.Every, e.Until, string(reminders), autoRemove,
		e.DayStart, e.Topic, e.Priority, e.Tags, e.Owner, clock.Now().UTC().Format(dbTimeFormat))
	return err
}
