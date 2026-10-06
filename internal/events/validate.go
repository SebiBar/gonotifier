package events

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxReminders is how many reminders an event may have.
const MaxReminders = 10

var (
	validRepeats    = map[string]bool{Once: true, Daily: true, Weekly: true, Monthly: true, Yearly: true}
	validPriorities = map[string]bool{"": true, "min": true, "low": true, "default": true, "high": true, "urgent": true}
	reTopic         = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
)

// ValidationError lists user-facing problems with one or more events.
type ValidationError []string

func (v ValidationError) Error() string { return strings.Join(v, " ") }

// SplitReminders splits a comma/space/semicolon separated list, lowercasing and de-duplicating.
func SplitReminders(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		f = strings.ToLower(strings.TrimSpace(f))
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// Normalize tidies an event before it is validated and stored: it trims text,
// lowercases repeat/priority, drops values equal to the defaults, and de-duplicates
// and sorts reminders (shortest first).
func Normalize(e *Event) {
	e.Name = strings.TrimSpace(e.Name)
	e.Date = strings.TrimSpace(e.Date)
	if len(e.Date) == len("2006-01-02T15:04:05") && strings.Contains(e.Date, "T") {
		e.Date = e.Date[:16] // drop seconds some clients send
	}
	e.Repeat = strings.ToLower(strings.TrimSpace(e.Repeat))
	switch e.Repeat {
	case "none", "never", "once":
		e.Repeat = Once
	}
	if !e.IsRepeating() || e.Every <= 1 {
		e.Every = 0
	}
	e.Until = strings.TrimSpace(e.Until)
	if !e.IsRepeating() {
		e.Until = ""
	}
	rems := SplitReminders(strings.Join(e.Reminders, ","))
	for i, rem := range rems {
		if dur, _, err := ParseOffset(rem); err == nil && dur == 0 {
			rems[i] = "0m" // "on time": 0d, 0h and 0m all mean the same
		}
	}
	e.Reminders = slices.Compact(slices.Sorted(slices.Values(rems)))
	sort.SliceStable(e.Reminders, func(i, j int) bool {
		a, _, _ := ParseOffset(e.Reminders[i])
		b, _, _ := ParseOffset(e.Reminders[j])
		return a < b
	})
	e.DayStart = strings.TrimSpace(e.DayStart)
	if e.HasTime() {
		e.DayStart = "" // only all-day events use it
	}
	e.Topic = strings.TrimSpace(e.Topic)
	e.Tags = strings.TrimSpace(e.Tags)
	e.Priority = strings.ToLower(strings.TrimSpace(e.Priority))
	if e.Priority == "default" {
		e.Priority = ""
	}
	if e.AutoRemove != nil && *e.AutoRemove {
		e.AutoRemove = nil // true is the default
	}
}

// Validate checks a normalized event. existing are the other stored events;
// selfID (the event being edited, or "") is excluded from the duplicate check.
func Validate(e Event, existing []Event, selfID string) []string {
	var errs []string
	if e.Name == "" {
		errs = append(errs, "Name is required.")
	} else if utf8.RuneCountInString(e.Name) > 100 {
		errs = append(errs, "Name must be at most 100 characters.")
	}

	start, dateErr := ParseDate(e.Date, time.UTC)
	switch {
	case e.Date == "":
		errs = append(errs, "Date is required.")
	case dateErr != nil:
		errs = append(errs, "Date: "+dateErr.Error()+".")
	}

	if !validRepeats[e.Repeat] {
		errs = append(errs, "Repeat must be daily, weekly, monthly or yearly.")
	}
	if e.Every < 0 || e.Every > 1000 {
		errs = append(errs, "Repeat interval must be between 1 and 1000.")
	}
	if e.Until != "" {
		if !reDate.MatchString(e.Until) {
			errs = append(errs, "Until must be a date (YYYY-MM-DD).")
		} else if u, err := ParseDate(e.Until, time.UTC); err != nil {
			errs = append(errs, "Until: "+err.Error()+".")
		} else if dateErr == nil && u.Before(start.Truncate(24*time.Hour)) {
			errs = append(errs, "Until must not be before the date.")
		}
	}

	if len(e.Reminders) == 0 {
		errs = append(errs, "Pick at least one reminder.")
	} else if len(e.Reminders) > MaxReminders {
		errs = append(errs, fmt.Sprintf("Pick at most %d reminders.", MaxReminders))
	}
	for _, rem := range e.Reminders {
		dur, _, err := ParseOffset(rem)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Invalid reminder %q: use a number of minutes, hours or days, e.g. 30m, 2h, 3d.", rem))
			continue
		}
		// A reminder must come after the previous occurrence, or it would fire "for"
		// the wrong one (e.g. a 7-day reminder on a daily event).
		if p := e.periodDays(); p > 0 && validRepeats[e.Repeat] {
			if dur >= time.Duration(p)*24*time.Hour {
				errs = append(errs, fmt.Sprintf("Reminder %q must be shorter than the repeat interval (%s).",
					ReminderLabel(rem), e.DescribeInterval()))
			}
		}
	}

	if e.DayStart != "" {
		if _, err := ParseTimeOnly(e.DayStart); err != nil {
			errs = append(errs, "Day start must be HH:MM.")
		}
	}
	if e.Topic != "" && !reTopic.MatchString(e.Topic) {
		errs = append(errs, "Topic may only contain letters, digits, - and _ (max 64).")
	}
	if !validPriorities[e.Priority] {
		errs = append(errs, "Priority must be one of min, low, default, high, urgent.")
	}
	for _, other := range existing {
		if other.ID != selfID && other.Name == e.Name && other.Date == e.Date {
			errs = append(errs, "An event with this name and date already exists.")
			break
		}
	}
	return errs
}

// CheckTiming reports what is already in the past when saving e at now: the date of a
// one-time event, the end of a repeat, and reminders that should already have fired.
// prev is the stored event when editing (nil when creating); only what changed is
// checked, so an event that has passed can still be edited and a reminder that was
// already sent can stay. Run it after Validate passes.
func CheckTiming(e Event, prev *Event, now time.Time, loc *time.Location, def TimeOnly) []string {
	dateChanged := prev == nil || prev.Date != e.Date || prev.Repeat != e.Repeat
	if e.IsRepeating() {
		untilChanged := dateChanged || prev.Until != e.Until || prev.Every != e.Every
		if _, ok := e.NextOccurrence(now, loc); !ok && untilChanged {
			return []string{"The repeat has already ended: pick a later Until date."}
		}
		return nil // every later occurrence gets its reminders (each is shorter than the interval)
	}

	occ, ok := e.NextOccurrence(now, loc)
	if !ok {
		if dateChanged {
			return []string{"This event has already passed."}
		}
		return nil
	}
	var errs []string
	for _, rem := range e.Reminders {
		if !dateChanged && slices.Contains(prev.Reminders, rem) {
			continue
		}
		if fire, err := FireTime(e, rem, occ, def); err == nil && fire.Before(now) {
			errs = append(errs, fmt.Sprintf("Reminder %q would have been sent %s, which has already passed.",
				ReminderLabel(rem), fire.Format("Jan 2 at 15:04")))
		}
	}
	return errs
}
