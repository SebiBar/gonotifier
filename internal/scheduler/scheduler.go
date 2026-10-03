// Package scheduler sends reminders when they are due.
//
// Run sleeps until the next reminder's fire time and wakes immediately when events
// change (Wake). It also re-plans at least every minute, which absorbs wall-clock
// jumps (NTP sync after boot on a Raspberry Pi without an RTC, DST, suspend).
// Each pass sends everything due, which also catches up after downtime: a
// reminder is still sent if it is at most CATCHUP_WINDOW late.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/config"
	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/notify"
	"github.com/sebibar/gonotifier/internal/store"
)

const (
	replanInterval   = time.Minute // longest sleep between passes
	housekeepEvery   = time.Hour   // auto-remove + history pruning
	historyRetention = 365 * 24 * time.Hour
	maxScan          = 10000 // occurrences examined per event per pass (safety bound)

	lateAfter  = 10 * time.Minute // sent later than this → "delayed" wording
	retryEvery = 5 * time.Minute  // wait between attempts when ntfy fails
)

type Scheduler struct {
	cfg   *config.Config
	store *store.Store
	wake  chan struct{}

	mu            sync.Mutex // one pass at a time
	lastHousekeep time.Time
	failedAt      map[string]time.Time // reminder key → last failed send (in memory)
	exported      uint64               // store version last written to the export files

	lastCheck atomic.Value // time.Time
	next      atomic.Value // time.Time; zero = nothing scheduled
}

func New(cfg *config.Config, st *store.Store) *Scheduler {
	return &Scheduler{
		cfg:      cfg,
		store:    st,
		wake:     make(chan struct{}, 1),
		failedAt: map[string]time.Time{},
		exported: math.MaxUint64, // forces the first export
	}
}

// Wake makes Run re-plan now. Call it after events change. Never blocks.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// LastCheck returns when the last pass completed.
func (s *Scheduler) LastCheck() (time.Time, bool) {
	t, ok := s.lastCheck.Load().(time.Time)
	return t, ok && !t.IsZero()
}

// Next returns the next scheduled reminder time, if any.
func (s *Scheduler) Next() (time.Time, bool) {
	t, ok := s.next.Load().(time.Time)
	return t, ok && !t.IsZero()
}

// Run executes passes until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		sleep := replanInterval
		if _, next, err := s.Check(false); err != nil {
			slog.Error("check failed", "err", err)
		} else if !next.IsZero() {
			if d := next.Sub(clock.Now()); d < sleep {
				sleep = max(d, 0)
			}
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.wake:
			timer.Stop()
		}
	}
}

// Check runs one pass: it sends every due reminder and returns how many were sent
// (with dryRun: would be sent, and nothing is sent or recorded) and the next
// fire time after now (zero if none).
func (s *Scheduler) Check(dryRun bool) (int, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	evs, err := s.store.AllEvents()
	if err != nil {
		return 0, time.Time{}, err
	}
	now := clock.Now().In(s.cfg.TZ)
	sent := 0
	var next time.Time

	for _, e := range evs {
		if e.Owner == "" {
			continue // from before users existed: sent once someone logs in and owns it
		}
		lead := e.MaxLead()
		scanned := 0
		// Any reminder still worth sending fires at ≥ now-catchup, and fires never
		// come after their occurrence, so earlier occurrences can be skipped.
		for occ := range e.Occurrences(now.Add(-s.cfg.CatchupWindow), s.cfg.TZ) {
			// Fires of this occurrence are ≥ occ-lead: once that is past both "now"
			// (nothing due) and the best next time found, later occurrences can't matter.
			if earliest := occ.Add(-lead); earliest.After(now) && !next.IsZero() && earliest.After(next) {
				break
			}
			if scanned++; scanned > maxScan {
				break
			}
			for _, off := range e.Reminders {
				fire, err := events.FireTime(e, off, occ, s.cfg.DefaultNotifyTime)
				if err != nil {
					continue
				}
				if fire.After(now) {
					if next.IsZero() || fire.Before(next) {
						next = fire
					}
					continue
				}
				if now.Sub(fire) > s.cfg.CatchupWindow {
					continue // too late to be useful
				}
				ok, err := s.deliver(e, off, occ, fire, now, dryRun)
				if err != nil {
					return sent, next, err
				}
				if ok {
					sent++
				}
			}
		}
	}

	if !dryRun {
		if now.Sub(s.lastHousekeep) >= housekeepEvery {
			s.housekeep(now)
			s.lastHousekeep = now
		}
		s.writeExport()
		s.lastCheck.Store(now)
		s.next.Store(next)
	}
	return sent, next, nil
}

// deliver sends one due reminder unless it was already sent, or its last send failed
// less than retryEvery ago. It reports whether a notification went out (or would, in a dry run).
func (s *Scheduler) deliver(e events.Event, off string, occ, fire, now time.Time, dryRun bool) (bool, error) {
	date := occ.Format("2006-01-02")
	key := store.ReminderKey(e.ID, off, date)
	already, err := s.store.IsSent(key)
	if err != nil || already {
		return false, err
	}
	if t, failed := s.failedAt[key]; failed && now.Sub(t) < retryEvery && !dryRun {
		return false, nil
	}

	title, msg, tags := notify.Format(e, off, occ)
	if now.Sub(fire) > lateAfter {
		msg = notify.FormatLate(e, occ, now)
	}
	if e.Tags != "" {
		tags = e.Tags
	}
	topic := e.Topic
	if topic == "" {
		topic = notify.DefaultTopic(e.Owner)
	}
	if dryRun {
		slog.Info("dry run: would send", "user", e.Owner, "event", e.Name, "offset", off, "topic", topic, "title", title, "message", msg)
		return true, nil
	}
	// Sent with the owner's own token, so ntfy only lets them post to their own topics.
	u, err := s.store.GetUser(e.Owner)
	if err != nil {
		return false, err
	}
	if err := (notify.Ntfy{URL: s.cfg.NtfyURL, Token: u.NtfyToken}).Send(topic, title, msg, e.Priority, tags); err != nil {
		// Not recorded, so it's retried every retryEvery while inside the catchup window.
		s.failedAt[key] = now
		slog.Error("ntfy send failed", "user", e.Owner, "event", e.Name, "offset", off, "topic", topic, "retry_in", retryEvery, "err", err)
		return false, nil
	}
	delete(s.failedAt, key)
	if err := s.store.RecordSent(store.Record{
		ID: key, Owner: e.Owner, EventID: e.ID, EventName: e.Name, Offset: off, TargetDate: date, FireTime: fire, Message: msg,
	}); err != nil {
		slog.Error("record sent failed", "event", e.Name, "err", err)
	}
	slog.Info("sent reminder", "user", e.Owner, "event", e.Name, "offset", off, "occurrence", date, "topic", topic)
	return true, nil
}

func (s *Scheduler) housekeep(now time.Time) {
	for key, t := range s.failedAt { // past the catchup window: will never be retried
		if now.Sub(t) > s.cfg.CatchupWindow {
			delete(s.failedAt, key)
		}
	}
	if n, err := s.AutoRemoveFinished(now); err != nil {
		slog.Error("auto-remove failed", "err", err)
	} else if n > 0 {
		slog.Info("auto-removed finished events", "count", n)
	}
	if err := s.store.Prune(historyRetention); err != nil {
		slog.Error("prune failed", "err", err)
	}
	if err := s.store.PruneSessions(); err != nil {
		slog.Error("prune sessions failed", "err", err)
	}
}

// AutoRemoveFinished deletes events that have auto-remove on, have no occurrence
// left (one-time events that passed, repeats past `until`), and have no reminder
// still waiting to be sent or retried.
func (s *Scheduler) AutoRemoveFinished(now time.Time) (int, error) {
	evs, err := s.store.AllEvents()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range evs {
		if !e.ShouldAutoRemove() {
			continue
		}
		if _, ok := e.NextOccurrence(now, s.cfg.TZ); ok {
			continue
		}
		pending, err := s.hasPending(e, now)
		if err != nil {
			return removed, err
		}
		if pending {
			continue
		}
		if err := s.store.DeleteEvent(e.Owner, e.ID); err != nil && err != store.ErrNotFound {
			return removed, err
		}
		slog.Info("auto-removed event", "event", e.Name, "date", e.Date)
		removed++
	}
	return removed, nil
}

// hasPending reports whether a reminder of e fired within the catchup window but
// hasn't been sent (e.g. ntfy was down), so it may still be retried.
func (s *Scheduler) hasPending(e events.Event, now time.Time) (bool, error) {
	for occ := range e.Occurrences(now.Add(-s.cfg.CatchupWindow), s.cfg.TZ) {
		for _, off := range e.Reminders {
			fire, err := events.FireTime(e, off, occ, s.cfg.DefaultNotifyTime)
			if err != nil || fire.After(now) || now.Sub(fire) > s.cfg.CatchupWindow {
				continue
			}
			sent, err := s.store.IsSent(store.ReminderKey(e.ID, off, occ.Format("2006-01-02")))
			if err != nil {
				return false, err
			}
			if !sent {
				return true, nil
			}
		}
	}
	return false, nil
}

// writeExport rewrites each user's read-only JSON snapshot (<ExportDir>/<username>.json)
// when events changed since the last write. Edits to the files are ignored; a user can
// restore theirs through /api/import.
func (s *Scheduler) writeExport() {
	if s.cfg.ExportDir == "" {
		return
	}
	v := s.store.Version()
	if v == s.exported {
		return
	}
	users, err := s.store.Usernames()
	if err != nil {
		slog.Error("export: list users", "err", err)
		return
	}
	for _, name := range users {
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			continue // not a safe file name
		}
		evs, err := s.store.ListEvents(name)
		if err != nil {
			slog.Error("export: list events", "user", name, "err", err)
			return
		}
		data, err := events.Encode(evs)
		if err != nil {
			slog.Error("export: encode", "user", name, "err", err)
			return
		}
		path := filepath.Join(s.cfg.ExportDir, name+".json")
		if err := writeFileAtomic(path, data); err != nil {
			slog.Error("export: write", "path", path, "err", err)
			return
		}
	}
	s.exported = v
}

// writeFileAtomic writes via a temp file + rename so readers never see a partial file.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
