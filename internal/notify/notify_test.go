package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/testutil"
)

func TestFormatNotification(t *testing.T) {
	bday := events.Event{Name: "Mom's birthday", Date: "1960-03-15", Repeat: events.Yearly}
	at := time.Date(2027, 3, 15, 0, 0, 0, 0, testutil.Loc)
	cases := []struct {
		e                    events.Event
		off                  string
		at                   time.Time
		title, message, tags string
	}{
		{bday, "5d", at, "Birthday Reminder", "Mom's birthday is in 5 days (March 15)", "birthday,cake"},
		{bday, "1d", at, "Birthday Reminder", "Mom's birthday is tomorrow!", "birthday,cake"},
		{bday, "0d", at, "Happy Birthday!", "Happy birthday Mom!", "birthday,tada"},
		{events.Event{Name: "Dentist", Date: "2026-11-15T10:00"}, "12h", time.Date(2026, 11, 15, 10, 0, 0, 0, testutil.Loc),
			"Reminder", "Dentist in 12 hours (Nov 15 at 10:00)", "calendar"},
		{events.Event{Name: "Renew car insurance", Date: "2026-12-01"}, "0d", time.Date(2026, 12, 1, 0, 0, 0, 0, testutil.Loc),
			"Reminder", "Renew car insurance is today!", "bell"},
	}
	for _, c := range cases {
		title, msg, tags := Format(c.e, c.off, c.at)
		if title != c.title || msg != c.message || tags != c.tags {
			t.Errorf("%s/%s: got (%q, %q, %q)", c.e.Name, c.off, title, msg, tags)
		}
	}
}

func TestFormat_NonYearlyRepeatsUseOneTimeWording(t *testing.T) {
	rent := events.Event{Name: "Pay rent", Date: "2026-01-31", Repeat: events.Monthly}
	title, msg, tags := Format(rent, "1d", time.Date(2027, 4, 30, 0, 0, 0, 0, testutil.Loc))
	if title != "Reminder" || msg != "Pay rent is tomorrow (Apr 30)" || tags != "bell" {
		t.Errorf("monthly: (%q, %q, %q)", title, msg, tags)
	}
	annual := events.Event{Name: "Anniversary", Date: "2015-06-20", Repeat: events.Yearly}
	if title, msg, _ := Format(annual, "5d", time.Date(2027, 6, 20, 0, 0, 0, 0, testutil.Loc)); title != "Annual Reminder" ||
		msg != "Anniversary is in 5 days (June 20)" {
		t.Errorf("annual: (%q, %q)", title, msg)
	}
}

func TestFormatLate(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 20, 0, 0, testutil.Loc)
	timed := events.Event{Name: "Team standup", Date: "2026-09-01T09:30", Repeat: events.Daily}
	allDay := events.Event{Name: "Renew car insurance", Date: "2026-10-03"}
	cases := []struct {
		e    events.Event
		occ  time.Time
		want string
	}{
		{timed, time.Date(2026, 10, 2, 9, 30, 0, 0, testutil.Loc), "Team standup was yesterday at 09:30 (delayed reminder)"},
		{timed, time.Date(2026, 10, 3, 0, 10, 0, 0, testutil.Loc), "Team standup was today at 00:10 (delayed reminder)"},
		{timed, time.Date(2026, 10, 3, 9, 30, 0, 0, testutil.Loc), "Team standup is today at 09:30 (delayed reminder)"},
		{timed, time.Date(2026, 10, 4, 9, 30, 0, 0, testutil.Loc), "Team standup is tomorrow at 09:30 (delayed reminder)"},
		{timed, time.Date(2026, 9, 30, 9, 30, 0, 0, testutil.Loc), "Team standup was on Sep 30 at 09:30 (delayed reminder)"},
		{timed, time.Date(2026, 10, 8, 9, 30, 0, 0, testutil.Loc), "Team standup is in 5 days (Oct 8 at 09:30) (delayed reminder)"},
		{allDay, time.Date(2026, 10, 3, 0, 0, 0, 0, testutil.Loc), "Renew car insurance is today! (delayed reminder)"},
		{allDay, time.Date(2026, 10, 2, 0, 0, 0, 0, testutil.Loc), "Renew car insurance was yesterday (delayed reminder)"},
		{allDay, time.Date(2026, 10, 4, 0, 0, 0, 0, testutil.Loc), "Renew car insurance is tomorrow (Oct 4) (delayed reminder)"},
	}
	for _, c := range cases {
		if got := FormatLate(c.e, c.occ, now); got != c.want {
			t.Errorf("FormatLate(%v) = %q, want %q", c.occ, got, c.want)
		}
	}
}

func TestDefaultTopic(t *testing.T) {
	for user, want := range map[string]string{
		"sebi":                  "sebi_reminders",
		"ana.m@home":            "ana_m_home_reminders",
		strings.Repeat("a", 80): strings.Repeat("a", 54) + "_reminders",
	} {
		if got := DefaultTopic(user); got != want {
			t.Errorf("DefaultTopic(%q) = %q, want %q", user, got, want)
		}
	}
}
