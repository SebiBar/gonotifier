// Package ical renders the events as an iCalendar feed for calendar subscriptions.
package ical

import (
	"fmt"
	"strings"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/events"
)

// Generate produces a valid iCalendar string from all events.
//
// Structure:
//
//	BEGIN:VCALENDAR / VERSION:2.0 / PRODID / X-WR-CALNAME / X-WR-TIMEZONE
//	For each event:
//	  BEGIN:VEVENT
//	  UID:{id}@gonotifier
//	  SUMMARY:{name}
//	  DTSTART: (date-only → VALUE=DATE:YYYYMMDD, datetime → TZID=...:YYYYMMDDTHHMMSS)
//	  For repeats: RRULE:FREQ=DAILY|WEEKLY|MONTHLY|YEARLY[;INTERVAL=n][;UNTIL=…]
//	  For each reminder: VALARM with UID and TRIGGER relative to the start, matching
//	  events.FireTime: -PT30M (30 minutes before), PT0S (at the time), and for all-day
//	  events the notify time: -PT15H (the day before at 09:00), PT9H (on the day at 09:00)
//	  END:VEVENT
//	END:VCALENDAR
//
// def is the notify time of all-day events that don't set their own.
// Calendar apps subscribe to /feed/<token>.ics and poll it every few hours.
func Generate(evs []events.Event, loc *time.Location, def events.TimeOnly) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(foldLine(s)) }

	stamp := clock.Now().UTC().Format("20060102T150405Z")
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//gonotifier//gonotifier//EN")
	line("CALSCALE:GREGORIAN")
	line("METHOD:PUBLISH")
	line("X-WR-CALNAME:gonotifier")
	line("X-WR-TIMEZONE:" + loc.String())

	for _, e := range evs {
		start, err := e.Start(loc)
		if err != nil {
			continue
		}
		line("BEGIN:VEVENT")
		line("UID:" + e.ID + "@gonotifier")
		line("DTSTAMP:" + stamp)
		line("SUMMARY:" + escapeText(e.Name))
		if e.HasTime() {
			line(fmt.Sprintf("DTSTART;TZID=%s:%s", loc.String(), start.Format("20060102T150405")))
			line("DURATION:PT1H")
		} else {
			line("DTSTART;VALUE=DATE:" + start.Format("20060102"))
			line("DTEND;VALUE=DATE:" + start.AddDate(0, 0, 1).Format("20060102"))
			line("TRANSP:TRANSPARENT")
		}
		if rule := rrule(e, start, loc); rule != "" {
			line("RRULE:" + rule)
		}
		for _, off := range e.Reminders {
			trigger, err := icalTrigger(e, off, def)
			if err != nil {
				continue
			}
			line("BEGIN:VALARM")
			line("UID:" + e.ID + "-" + off + "@gonotifier") // RFC 9074, so clients keep each alarm
			line("ACTION:DISPLAY")
			line("DESCRIPTION:" + escapeText(e.Name))
			line("TRIGGER:" + trigger)
			line("END:VALARM")
		}
		line("END:VEVENT")
	}
	line("END:VCALENDAR")
	return b.String()
}

// rrule renders the repeat rule. Month-end dates use BYMONTHDAY+BYSETPOS=-1 so
// calendars clamp them to the last day of shorter months, like gonotifier does
// (plain BYMONTHDAY=31 would skip those months).
func rrule(e events.Event, start time.Time, loc *time.Location) string {
	freq := map[string]string{events.Daily: "DAILY", events.Weekly: "WEEKLY", events.Monthly: "MONTHLY", events.Yearly: "YEARLY"}[e.Repeat]
	if freq == "" {
		return ""
	}
	r := "FREQ=" + freq
	if n := e.Interval(); n > 1 {
		r += fmt.Sprintf(";INTERVAL=%d", n)
	}
	switch d := start.Day(); {
	case e.Repeat == events.Monthly && d > 28:
		days := make([]string, 0, 4)
		for i := 28; i <= d; i++ {
			days = append(days, fmt.Sprint(i))
		}
		r += ";BYMONTHDAY=" + strings.Join(days, ",") + ";BYSETPOS=-1"
	case e.Repeat == events.Yearly && start.Month() == time.February && d == 29:
		r += ";BYMONTH=2;BYMONTHDAY=28,29;BYSETPOS=-1"
	}
	if e.Until != "" {
		if u, err := events.ParseDate(e.Until, loc); err == nil {
			if e.HasTime() {
				// With a TZID start, UNTIL must be UTC (RFC 5545): the end of the until day.
				r += ";UNTIL=" + u.AddDate(0, 0, 1).Add(-time.Second).UTC().Format("20060102T150405Z")
			} else {
				r += ";UNTIL=" + u.Format("20060102")
			}
		}
	}
	return r
}

// icalTrigger returns when a reminder fires relative to the event's start, as an RFC 5545
// duration, matching events.FireTime: exactly the offset before a timed event, and for an
// all-day event (which starts at 00:00) N days before at the notify time.
func icalTrigger(e events.Event, offset string, def events.TimeOnly) (string, error) {
	dur, _, err := events.ParseOffset(offset)
	if err != nil {
		return "", err
	}
	rel := -dur
	if !e.HasTime() {
		nt := def
		if t, err := events.ParseTimeOnly(e.NotifyTime); err == nil {
			nt = t
		}
		days := dur / (24 * time.Hour)
		rel = -days*24*time.Hour + time.Duration(nt.Hour)*time.Hour + time.Duration(nt.Minute)*time.Minute
	}
	return formatDuration(rel), nil
}

// formatDuration renders d as an RFC 5545 duration: -P1DT2H30M, PT9H, PT0S.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "PT0S"
	}
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	days, h, m := d/(24*time.Hour), (d%(24*time.Hour))/time.Hour, (d%time.Hour)/time.Minute
	s := sign + "P"
	if days > 0 {
		s += fmt.Sprintf("%dD", days)
	}
	if h > 0 || m > 0 {
		s += "T"
		if h > 0 {
			s += fmt.Sprintf("%dH", h)
		}
		if m > 0 {
			s += fmt.Sprintf("%dM", m)
		}
	}
	return s
}

var icalEscaper = strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`)

func escapeText(s string) string { return icalEscaper.Replace(s) }

// foldLine terminates a content line with CRLF, folding at 75 octets
// without splitting UTF-8 sequences (RFC 5545 §3.1).
func foldLine(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		size := len(string(r))
		if n+size > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += size
	}
	b.WriteString("\r\n")
	return b.String()
}
