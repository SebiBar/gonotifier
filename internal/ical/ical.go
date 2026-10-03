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
//	  For each reminder: VALARM with TRIGGER:-P{days}D or -PT{hours}H{minutes}M
//	  END:VEVENT
//	END:VCALENDAR
//
// Google Calendar subscribes to /feed.ics and polls every 12-24h.
func Generate(evs []events.Event, loc *time.Location) string {
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
			trigger, err := icalTrigger(off)
			if err != nil {
				continue
			}
			line("BEGIN:VALARM")
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

// icalTrigger converts an offset into an RFC 5545 duration before the event start.
func icalTrigger(offset string) (string, error) {
	dur, unit, err := events.ParseOffset(offset)
	if err != nil {
		return "", err
	}
	if dur == 0 {
		return "PT0S", nil
	}
	if unit == "d" {
		return fmt.Sprintf("-P%dD", int(dur/(24*time.Hour))), nil
	}
	h := int(dur / time.Hour)
	m := int((dur % time.Hour) / time.Minute)
	s := "-PT"
	if h > 0 {
		s += fmt.Sprintf("%dH", h)
	}
	if m > 0 {
		s += fmt.Sprintf("%dM", m)
	}
	return s, nil
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
