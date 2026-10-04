// Package events defines the Event type and the pure date logic around it:
// recurrence, reminder fire times, validation, and the import/export format.
// It does no I/O; storage lives in package store.
package events

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repeat rules.
const (
	Once    = ""
	Daily   = "daily"
	Weekly  = "weekly"
	Monthly = "monthly"
	Yearly  = "yearly"
)

type Event struct {
	ID         string   `json:"id,omitempty"`
	Name       string   `json:"name"`
	Date       string   `json:"date"`                  // YYYY-MM-DD or YYYY-MM-DDTHH:MM (first occurrence)
	Repeat     string   `json:"repeat,omitempty"`      // "", daily, weekly, monthly, yearly
	Every      int      `json:"every,omitempty"`       // repeat interval; 0 means 1
	Until      string   `json:"until,omitempty"`       // last day (YYYY-MM-DD) a repeat may fall on
	Reminders  []string `json:"reminders"`             // before the event; see FireTime
	AutoRemove *bool    `json:"auto_remove,omitempty"` // nil → true
	NotifyTime string   `json:"notify_time,omitempty"` // all-day events: when their reminders are sent
	Topic      string   `json:"topic,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Tags       string   `json:"tags,omitempty"`
	Owner      string   `json:"-"` // username; set by the store, never taken from input
}

var (
	reDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$`)
	reOffset   = regexp.MustCompile(`^(\d+)([dhm])$`)
)

// TimeOnly is a wall-clock time of day (HH:MM).
type TimeOnly struct {
	Hour   int
	Minute int
}

// ParseTimeOnly parses "HH:MM" (24h).
func ParseTimeOnly(s string) (TimeOnly, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return TimeOnly{}, fmt.Errorf("invalid time %q (want HH:MM)", s)
	}
	return TimeOnly{Hour: t.Hour(), Minute: t.Minute()}, nil
}

func (t TimeOnly) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// HasTime reports whether the event happens at a specific time (otherwise it is all-day).
func (e Event) HasTime() bool { return strings.Contains(e.Date, "T") }

// IsRepeating reports whether the event has a repeat rule.
func (e Event) IsRepeating() bool { return e.Repeat != Once }

// Interval returns the repeat interval (every N units), at least 1.
func (e Event) Interval() int {
	if e.Every < 1 {
		return 1
	}
	return e.Every
}

// ShouldAutoRemove reports whether the event is deleted once it has finished
// (a one-time event that passed, or a repeat whose `until` passed).
func (e Event) ShouldAutoRemove() bool { return e.AutoRemove == nil || *e.AutoRemove }

// Start returns the first occurrence.
func (e Event) Start(loc *time.Location) (time.Time, error) {
	return ParseDate(e.Date, loc)
}

// ParseDate parses YYYY-MM-DD (00:00) or YYYY-MM-DDTHH:MM in loc.
func ParseDate(s string, loc *time.Location) (time.Time, error) {
	switch {
	case reDate.MatchString(s):
		if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
			return t, nil
		}
	case reDateTime.MatchString(s):
		if t, err := time.ParseInLocation("2006-01-02T15:04", s, loc); err == nil {
			return t, nil
		}
	default:
		return time.Time{}, fmt.Errorf("invalid date %q (want YYYY-MM-DD or YYYY-MM-DDTHH:MM)", s)
	}
	return time.Time{}, fmt.Errorf("invalid date %q", s)
}

// ParseOffset parses "5d" → 5 days, "12h" → 12 hours, "30m" → 30 minutes.
// Returns (duration, unit letter, error); the unit decides day-vs-exact semantics.
func ParseOffset(s string) (time.Duration, string, error) {
	m := reOffset.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, "", fmt.Errorf("invalid reminder offset %q (want e.g. 5d, 12h, 30m)", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n > 100000 {
		return 0, "", fmt.Errorf("reminder offset %q out of range", s)
	}
	unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
	return time.Duration(n) * unit, m[2], nil
}

// HumanOffset renders a duration in the offset's unit: "1 day", "12 hours", "30 minutes".
func HumanOffset(dur time.Duration, unit string) string {
	var n int
	var word string
	switch days := int(dur / (24 * time.Hour)); {
	case unit == "d" && days > 0 && days%7 == 0:
		n, word = days/7, "week" // like the form's "1 week", "2 weeks"
	case unit == "d":
		n, word = days, "day"
	case unit == "h":
		n, word = int(dur/time.Hour), "hour"
	default:
		n, word = int(dur/time.Minute), "minute"
	}
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ReminderLabel describes a reminder the way the form does: "At time", "On the day",
// "3 days before", "30 minutes before". Invalid offsets are returned as they are.
func ReminderLabel(offset string) string {
	dur, unit, err := ParseOffset(offset)
	switch {
	case err != nil:
		return offset
	case dur == 0 && unit == "d":
		return "On the day"
	case dur == 0:
		return "At time"
	}
	return HumanOffset(dur, unit) + " before"
}

// FireTime calculates when a reminder for the occurrence at occ fires, the way calendar apps do:
//
//	event at a time:  exactly that long before it (1d = 24 hours before; 0m = at the event's time)
//	all-day event:    N days before, at the notify time (0d = on the day)
//
// All-day events only take day offsets (see Validate). The notify time is the event's
// notify_time, else def.
func FireTime(e Event, offset string, occ time.Time, def TimeOnly) (time.Time, error) {
	dur, _, err := ParseOffset(offset)
	if err != nil {
		return time.Time{}, err
	}
	if e.HasTime() {
		return occ.Add(-dur), nil
	}
	y, m, d := occ.Date()
	days := int(dur / (24 * time.Hour))
	nt := def
	if e.NotifyTime != "" {
		if t, err := ParseTimeOnly(e.NotifyTime); err == nil {
			nt = t
		}
	}
	return time.Date(y, m, d-days, nt.Hour, nt.Minute, 0, 0, occ.Location()), nil
}

// MaxLead returns an upper bound on how long before an occurrence any of the
// event's reminders can fire.
func (e Event) MaxLead() time.Duration {
	var max time.Duration
	for _, off := range e.Reminders {
		dur, _, err := ParseOffset(off)
		if err != nil {
			continue
		}
		if dur > max {
			max = dur
		}
	}
	return max
}
