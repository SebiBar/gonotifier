package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/testutil"
)

// Clock is frozen at testutil.FixedNow: Fri 2 Oct 2026, 10:00 (America/New_York).

func setup(t *testing.T, doc string) (*Scheduler, *testutil.Env) {
	env := testutil.NewEnv(t, doc)
	return New(env.Cfg, env.Store), env
}

func check(t *testing.T, s *Scheduler) (int, time.Time) {
	t.Helper()
	n, next, err := s.Check(false)
	if err != nil {
		t.Fatal(err)
	}
	return n, next
}

func TestCheck_SendsDue(t *testing.T) {
	// At 09:05 both "1d" reminders (fired 09:00 today) are due and only 5 minutes late.
	s, env := setup(t, `[
	{"name": "Dentist", "date": "2026-10-03T10:00", "reminders": ["1d"], "priority": "high"},
	{"name": "Mom's birthday", "date": "1960-10-03", "repeat": "yearly", "reminders": ["1d"], "topic": "family"}
]`)
	testutil.FreezeClock(t, time.Date(2026, 10, 2, 9, 5, 0, 0, testutil.Loc))
	if n, _ := check(t, s); n != 2 {
		t.Fatalf("sent %d, want 2: %+v", n, env.Ntfy.All())
	}
	byTopic := map[string]testutil.Notification{}
	for _, n := range env.Ntfy.All() {
		byTopic[n.Topic] = n
	}
	d := byTopic["alice_reminders"] // the owner's default topic
	if d.Title != "Reminder" || d.Body != "Dentist is tomorrow at 10:00" || d.Priority != "high" ||
		d.Tags != "calendar" || d.Auth != "Bearer "+testutil.UserToken {
		t.Errorf("dentist notification: %+v", d)
	}
	if b := byTopic["family"]; b.Title != "Birthday Reminder" || b.Body != "Mom's birthday is tomorrow!" {
		t.Errorf("birthday notification: %+v", b)
	}
	if hist, _ := env.Store.RecentHistory(testutil.User, 10); len(hist) != 2 {
		t.Errorf("history has %d records", len(hist))
	}
}

func TestCheck_LateReminderSaysSo(t *testing.T) {
	// Standup at 09:30 with a 10m reminder (09:20); the server only comes back at 00:20 next day.
	s, env := setup(t, `[{"name":"Team standup","date":"2026-10-02T09:30","reminders":["10m"]}]`)
	testutil.FreezeClock(t, time.Date(2026, 10, 3, 0, 20, 0, 0, testutil.Loc))
	if n, _ := check(t, s); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	if got := env.Ntfy.All()[0]; got.Body != "Team standup was yesterday at 09:30 (delayed reminder)" || got.Title != "Reminder" {
		t.Errorf("late notification = %+v", got)
	}
}

func TestCheck_ReturnsNextFireTime(t *testing.T) {
	s, env := setup(t, `[
	{"name": "Later", "date": "2026-10-10T10:00", "reminders": ["1d", "12h"]},
	{"name": "Yearly", "date": "2020-03-15", "repeat": "yearly", "reminders": ["1d"]}
]`)
	n, next := check(t, s)
	if n != 0 || len(env.Ntfy.All()) != 0 {
		t.Errorf("nothing should be due, sent %d", n)
	}
	// Earliest future fire: "Later" 1d → Oct 9 09:00.
	if want := time.Date(2026, 10, 9, 9, 0, 0, 0, testutil.Loc); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
	if got, ok := s.Next(); !ok || !got.Equal(next) {
		t.Errorf("Next() = %v %v", got, ok)
	}
}

func TestCheck_NextLooksPastFirstOccurrence(t *testing.T) {
	// Today's standup (09:30) is over; next reminder is tomorrow's 1h-before at 08:30.
	s, _ := setup(t, `[
	{"name": "Standup", "date": "2026-09-01T09:30", "repeat": "daily", "reminders": ["1h"]}
]`)
	_, next := check(t, s)
	if want := time.Date(2026, 10, 3, 8, 30, 0, 0, testutil.Loc); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestCheck_DoesNotResend(t *testing.T) {
	s, env := setup(t, `[{"name":"Dentist","date":"2026-10-03T10:00","reminders":["1d"]}]`)
	for i := 0; i < 3; i++ {
		check(t, s)
	}
	if got := len(env.Ntfy.All()); got != 1 {
		t.Errorf("sent %d times, want 1", got)
	}
}

func TestCheck_RenameDoesNotResend(t *testing.T) {
	s, env := setup(t, `[{"name":"Dentist","date":"2026-10-03T10:00","reminders":["1d"]}]`)
	check(t, s)
	id := env.ID(t, "Dentist")
	e, _ := env.Store.GetEvent(testutil.User, id)
	e.Name = "Dentist (Dr. Smith)"
	if _, err := env.Store.UpdateEvent(testutil.User, id, e); err != nil {
		t.Fatal(err)
	}
	check(t, s)
	if got := len(env.Ntfy.All()); got != 1 {
		t.Errorf("rename caused a resend: %d notifications", got)
	}
}

func TestCheck_SkipsOutsideCatchup(t *testing.T) {
	// 7d reminder fired Sep 26, far outside the 24h window.
	s, env := setup(t, `[{"name":"Old","date":"2026-10-03T10:00","reminders":["7d"]}]`)
	if n, _ := check(t, s); n != 0 || len(env.Ntfy.All()) != 0 {
		t.Errorf("sent a reminder outside the catchup window")
	}
}

func TestCheck_RetriesAfterFailure(t *testing.T) {
	s, env := setup(t, `[{"name":"Dentist","date":"2026-10-03T10:00","reminders":["1d"]}]`)
	good := s.cfg.NtfyURL
	s.cfg.NtfyURL = "http://127.0.0.1:1" // nothing listening
	if n, _ := check(t, s); n != 0 {
		t.Fatalf("sent %d while ntfy was down", n)
	}
	s.cfg.NtfyURL = good
	testutil.FreezeClock(t, testutil.FixedNow.Add(retryEvery))
	if n, _ := check(t, s); n != 1 || len(env.Ntfy.All()) != 1 {
		t.Errorf("retry sent %d", n)
	}
}

func TestCheck_RetriesEveryFiveMinutes(t *testing.T) {
	s, _ := setup(t, `[{"name":"Dentist","date":"2026-10-03T10:00","reminders":["1d"]}]`)
	good := s.cfg.NtfyURL
	s.cfg.NtfyURL = "http://127.0.0.1:1"
	check(t, s) // fails at 10:00

	s.cfg.NtfyURL = good
	testutil.FreezeClock(t, testutil.FixedNow.Add(4*time.Minute))
	if n, _ := check(t, s); n != 0 {
		t.Fatal("retried before 5 minutes passed")
	}
	testutil.FreezeClock(t, testutil.FixedNow.Add(5*time.Minute))
	if n, _ := check(t, s); n != 1 {
		t.Fatal("not retried after 5 minutes")
	}
	if len(s.failedAt) != 0 {
		t.Errorf("failure not cleared after success: %v", s.failedAt)
	}
}

func TestCheck_YearWraparound(t *testing.T) {
	s, env := setup(t, `[{"name":"Ana's birthday","date":"2000-01-10","repeat":"yearly","reminders":["10d"]}]`)
	testutil.FreezeClock(t, time.Date(2026, 12, 31, 9, 0, 0, 0, testutil.Loc))
	if n, _ := check(t, s); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	if body := env.Ntfy.All()[0].Body; body != "Ana's birthday is in 10 days (January 10)" {
		t.Errorf("body = %q", body)
	}
}

func TestCheck_MonthlyOnMonthEnd(t *testing.T) {
	// Repeats on the 31st → April's occurrence is Apr 30; "1d" fires Apr 29 09:00.
	s, env := setup(t, `[{"name":"Pay rent","date":"2026-01-31","repeat":"monthly","reminders":["1d"]}]`)
	testutil.FreezeClock(t, time.Date(2027, 4, 29, 9, 0, 0, 0, testutil.Loc))
	if n, _ := check(t, s); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	if body := env.Ntfy.All()[0].Body; body != "Pay rent is tomorrow (Apr 30)" {
		t.Errorf("body = %q", body)
	}
}

func TestCheck_DailyRepeatsEveryDay(t *testing.T) {
	s, env := setup(t, `[{"name":"Pills","date":"2026-09-01T08:00","repeat":"daily","reminders":["0d"]}]`)
	check(t, s) // today 00:00
	testutil.FreezeClock(t, testutil.FixedNow.AddDate(0, 0, 1))
	check(t, s) // tomorrow 00:00
	if got := len(env.Ntfy.All()); got != 2 {
		t.Errorf("sent %d, want one per day", got)
	}
}

func TestCheck_DryRun(t *testing.T) {
	s, env := setup(t, `[{"name":"Dentist","date":"2026-10-03T10:00","reminders":["1d"]}]`)
	n, _, err := s.Check(true)
	if err != nil || n != 1 {
		t.Fatalf("dry run n=%d err=%v", n, err)
	}
	if len(env.Ntfy.All()) != 0 {
		t.Error("dry run sent a notification")
	}
	if hist, _ := env.Store.RecentHistory(testutil.User, 10); len(hist) != 0 {
		t.Error("dry run recorded history")
	}
	if _, err := os.Stat(env.Cfg.ExportDir); err == nil {
		t.Error("dry run wrote the export files")
	}
}

func TestAutoRemoveFinished(t *testing.T) {
	s, env := setup(t, `[
	{"name": "Done", "date": "2026-09-30T10:00", "reminders": ["1d"]},
	{"name": "Kept explicitly", "date": "2026-09-01", "reminders": ["1d"], "auto_remove": false},
	{"name": "Today all day", "date": "2026-10-02", "reminders": ["0d"]},
	{"name": "Future", "date": "2026-11-01", "reminders": ["1d"]},
	{"name": "Ended weekly", "date": "2026-08-01", "repeat": "weekly", "until": "2026-09-01", "reminders": ["1d"]},
	{"name": "Ongoing weekly", "date": "2026-08-01", "repeat": "weekly", "reminders": ["1d"]}
]`)
	n, err := s.AutoRemoveFinished(testutil.FixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	want := []string{"Kept explicitly", "Today all day", "Future", "Ongoing weekly"}
	if got := env.Names(t); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

func TestAutoRemoveFinished_WaitsForPendingReminder(t *testing.T) {
	// Event at 09:30 today, 30m reminder fired 09:00 but not sent yet → keep for retry.
	s, _ := setup(t, `[{"name":"Call","date":"2026-10-02T09:30","reminders":["30m"]}]`)
	good := s.cfg.NtfyURL
	s.cfg.NtfyURL = "http://127.0.0.1:1"
	check(t, s)
	if n, _ := s.AutoRemoveFinished(testutil.FixedNow); n != 0 {
		t.Fatal("removed an event with a pending reminder")
	}
	s.cfg.NtfyURL = good
	later := testutil.FixedNow.Add(retryEvery)
	testutil.FreezeClock(t, later)
	check(t, s)
	if n, _ := s.AutoRemoveFinished(later); n != 1 {
		t.Error("not removed after the reminder was sent")
	}
}

func TestExportFileFollowsChanges(t *testing.T) {
	s, env := setup(t, `[{"name":"Dentist","date":"2026-11-15T10:00","reminders":["1d"]}]`)
	check(t, s)
	exported := func() []string {
		data, err := os.ReadFile(filepath.Join(env.Cfg.ExportDir, testutil.User+".json"))
		if err != nil {
			t.Fatal(err)
		}
		evs, err := events.Decode(data) // the snapshot must be importable as-is
		if err != nil {
			t.Fatalf("export is not a valid document: %v\n%s", err, data)
		}
		var names []string
		for _, e := range evs {
			names = append(names, e.Name)
		}
		return names
	}
	if got := exported(); !slices.Equal(got, []string{"Dentist"}) {
		t.Fatalf("export = %v", got)
	}
	if _, err := env.Store.CreateEvent(testutil.User, events.Event{Name: "Flight", Date: "2026-12-20T06:30", Reminders: []string{"1d"}}); err != nil {
		t.Fatal(err)
	}
	check(t, s)
	if got := exported(); !slices.Equal(got, []string{"Dentist", "Flight"}) {
		t.Errorf("export not refreshed: %v", got)
	}
}

func TestRun_WakesImmediatelyOnChange(t *testing.T) {
	s, env := setup(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Wait for the first pass, then add an event that is due right now and wake the loop.
	waitFor(t, func() bool { _, ok := s.LastCheck(); return ok })
	if _, err := env.Store.CreateEvent(testutil.User, events.Event{Name: "Now", Date: "2026-10-02T10:30", Reminders: []string{"30m"}}); err != nil {
		t.Fatal(err)
	}
	s.Wake()
	waitFor(t, func() bool { return len(env.Ntfy.All()) == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 3s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCheck_EachUserWithOwnTokenAndTopic(t *testing.T) {
	s, env := setup(t, `[{"name":"Dentist","date":"2026-10-03T10:00","reminders":["1d"]}]`)
	if _, err := env.Store.CreateUser(testutil.Other, testutil.OtherToken); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.CreateEvent(testutil.Other, events.Event{Name: "Gym", Date: "2026-10-03T10:00", Reminders: []string{"1d"}, Topic: "family"}); err != nil {
		t.Fatal(err)
	}
	testutil.FreezeClock(t, time.Date(2026, 10, 2, 9, 5, 0, 0, testutil.Loc)) // both "1d" reminders fired at 09:00
	check(t, s)
	got := map[string]testutil.Notification{}
	for _, n := range env.Ntfy.All() {
		got[n.Body] = n
	}
	if n := got["Dentist is tomorrow at 10:00"]; n.Topic != "alice_reminders" || n.Auth != "Bearer "+testutil.UserToken {
		t.Errorf("alice's reminder: %+v", n)
	}
	if n := got["Gym is tomorrow at 10:00"]; n.Topic != "family" || n.Auth != "Bearer "+testutil.OtherToken {
		t.Errorf("bob's reminder: %+v", n)
	}
	if hist, _ := env.Store.RecentHistory(testutil.Other, 10); len(hist) != 1 || hist[0].EventName != "Gym" {
		t.Errorf("bob's history: %+v", hist)
	}
	for _, name := range []string{testutil.User, testutil.Other} {
		if _, err := os.Stat(filepath.Join(env.Cfg.ExportDir, name+".json")); err != nil {
			t.Errorf("no export file for %s: %v", name, err)
		}
	}
}
