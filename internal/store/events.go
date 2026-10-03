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

// ErrNotFound is returned when no event has the requested ID.
var ErrNotFound = errors.New("event not found")

var reID = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const eventColumns = `id, name, date, repeat, every, until, reminders, auto_remove, notify_time, topic, priority, tags`

type scanner interface{ Scan(...any) error }

func scanEvent(row scanner) (events.Event, error) {
	var e events.Event
	var reminders string
	var autoRemove sql.NullBool
	if err := row.Scan(&e.ID, &e.Name, &e.Date, &e.Repeat, &e.Every, &e.Until, &reminders, &autoRemove,
		&e.NotifyTime, &e.Topic, &e.Priority, &e.Tags); err != nil {
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

// ListEvents returns all events in creation order.
func (s *Store) ListEvents() ([]events.Event, error) {
	rows, err := s.db.Query(`SELECT ` + eventColumns + ` FROM events ORDER BY created_at, rowid`)
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

// GetEvent returns one event, or ErrNotFound.
func (s *Store) GetEvent(id string) (events.Event, error) {
	e, err := scanEvent(s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// CreateEvent normalizes, validates and stores a new event with a fresh ID.
// Validation failures are returned as events.ValidationError.
func (s *Store) CreateEvent(e events.Event) (events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.ListEvents()
	if err != nil {
		return e, err
	}
	events.Normalize(&e)
	if errs := events.Validate(e, existing, ""); len(errs) > 0 {
		return e, events.ValidationError(errs)
	}
	e.ID = newID()
	if err := upsert(s.db, e); err != nil {
		return e, err
	}
	s.version.Add(1)
	return e, nil
}

// UpdateEvent replaces the event with the given ID, keeping the ID.
func (s *Store) UpdateEvent(id string, e events.Event) (events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.GetEvent(id); err != nil {
		return e, err
	}
	existing, err := s.ListEvents()
	if err != nil {
		return e, err
	}
	events.Normalize(&e)
	if errs := events.Validate(e, existing, id); len(errs) > 0 {
		return e, events.ValidationError(errs)
	}
	e.ID = id
	if err := upsert(s.db, e); err != nil {
		return e, err
	}
	s.version.Add(1)
	return e, nil
}

// DeleteEvent removes an event. Its sent history is kept for the history view.
func (s *Store) DeleteEvent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM events WHERE id = ?`, id)
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

// Import adds or updates events in one transaction: an event whose id matches an
// existing one replaces it, anything else is created (keeping a valid given id).
// With replace, events missing from the import are deleted. Nothing is written
// if any event is invalid.
func (s *Store) Import(incoming []events.Event, replace bool) (ImportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res ImportResult
	current, err := s.ListEvents()
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
		if e.ID == "" {
			e.ID = newID()
		}
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
				if _, err := tx.Exec(`DELETE FROM events WHERE id = ?`, e.ID); err != nil {
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

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func upsert(db execer, e events.Event) error {
	reminders, _ := json.Marshal(e.Reminders)
	var autoRemove any
	if e.AutoRemove != nil {
		autoRemove = *e.AutoRemove
	}
	_, err := db.Exec(`INSERT INTO events (`+eventColumns+`, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, date = excluded.date, repeat = excluded.repeat, every = excluded.every,
			until = excluded.until, reminders = excluded.reminders, auto_remove = excluded.auto_remove,
			notify_time = excluded.notify_time, topic = excluded.topic, priority = excluded.priority,
			tags = excluded.tags`,
		e.ID, e.Name, e.Date, e.Repeat, e.Every, e.Until, string(reminders), autoRemove,
		e.NotifyTime, e.Topic, e.Priority, e.Tags, clock.Now().UTC().Format(dbTimeFormat))
	return err
}
