package events

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

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
	e.Reminders = SplitReminders(strings.Join(e.Reminders, ","))
	for i, rem := range e.Reminders {
		if dur, _, err := ParseOffset(rem); err == nil && dur == 0 {
			e.Reminders[i] = "0m" // "at the time": 0d, 0h and 0m all mean the same
			if !e.HasTime() {
				e.Reminders[i] = "0d" // "on the day"
			}
		}
	}
	e.Reminders = SplitReminders(strings.Join(e.Reminders, ","))
	sort.SliceStable(e.Reminders, func(i, j int) bool {
		a, _, _ := ParseOffset(e.Reminders[i])
		b, _, _ := ParseOffset(e.Reminders[j])
		return a < b
	})
	e.NotifyTime = strings.TrimSpace(e.NotifyTime)
	if e.HasTime() {
		e.NotifyTime = "" // only all-day events use it
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
	}
	for _, rem := range e.Reminders {
		dur, unit, err := ParseOffset(rem)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Invalid reminder %q (use e.g. 5d, 12h, 30m).", rem))
			continue
		}
		if unit != "d" && !e.HasTime() && dateErr == nil {
			errs = append(errs, fmt.Sprintf("Reminder %s: all-day events remind in days (e.g. 0d, 1d). Give the event a time to remind hours or minutes before.", rem))
			continue
		}
		// A reminder must come after the previous occurrence, or it would fire "for"
		// the wrong one (e.g. a 7-day reminder on a daily event).
		if p := e.periodDays(); p > 0 && validRepeats[e.Repeat] {
			if dur >= time.Duration(p)*24*time.Hour {
				errs = append(errs, fmt.Sprintf("Reminder %s is as long as the repeat interval (%s); pick a shorter one.",
					rem, strings.ToLower(e.DescribeRepeat())))
			}
		}
	}

	if e.NotifyTime != "" {
		if _, err := ParseTimeOnly(e.NotifyTime); err != nil {
			errs = append(errs, "Notify time must be HH:MM.")
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
