package ical

import (
	"strings"
	"testing"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/testutil"
)

func icalFor(t *testing.T, evs ...events.Event) string {
	t.Helper()
	testutil.FreezeClock(t, testutil.FixedNow)
	return Generate(evs, testutil.Loc, events.TimeOnly{Hour: 9})
}

// vevent returns the VEVENT block containing the given SUMMARY.
func vevent(t *testing.T, cal, summary string) string {
	t.Helper()
	for _, block := range strings.Split(cal, "BEGIN:VEVENT") {
		if strings.Contains(block, "SUMMARY:"+summary+"\r\n") {
			return block
		}
	}
	t.Fatalf("no VEVENT with SUMMARY %q in:\n%s", summary, cal)
	return ""
}

func TestGenerate_ValidStructure(t *testing.T) {
	cal := icalFor(t,
		events.Event{ID: "a1", Name: "Dentist", Date: "2026-11-15T10:00", Reminders: []string{"1d"}},
		events.Event{ID: "b2", Name: "Rent, utilities; misc", Date: "2026-12-01", Reminders: []string{"1d"}},
	)
	if !strings.HasPrefix(cal, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n") || !strings.HasSuffix(cal, "END:VCALENDAR\r\n") {
		t.Errorf("bad calendar envelope:\n%s", cal)
	}
	for _, want := range []string{"PRODID:", "X-WR-CALNAME:gonotifier", "X-WR-TIMEZONE:America/New_York", "UID:a1@gonotifier\r\n"} {
		if !strings.Contains(cal, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(cal, "BEGIN:VEVENT") != 2 || strings.Count(cal, "END:VEVENT") != 2 {
		t.Error("expected 2 VEVENTs")
	}
	if !strings.Contains(cal, `SUMMARY:Rent\, utilities\; misc`) {
		t.Error("text not escaped")
	}
	for _, line := range strings.Split(cal, "\r\n") {
		if len(line) > 75 {
			t.Errorf("line longer than 75 octets: %q", line)
		}
	}
}

func TestGenerate_DateFormats(t *testing.T) {
	cal := icalFor(t,
		events.Event{ID: "a", Name: "Insurance", Date: "2026-12-01", Reminders: []string{"1d"}},
		events.Event{ID: "b", Name: "Dentist", Date: "2026-11-15T10:00", Reminders: []string{"1d"}},
	)
	if ev := vevent(t, cal, "Insurance"); !strings.Contains(ev, "DTSTART;VALUE=DATE:20261201\r\n") || !strings.Contains(ev, "DTEND;VALUE=DATE:20261202\r\n") {
		t.Errorf("bad all-day DTSTART/DTEND:\n%s", ev)
	}
	if ev := vevent(t, cal, "Dentist"); !strings.Contains(ev, "DTSTART;TZID=America/New_York:20261115T100000\r\n") {
		t.Errorf("bad timed DTSTART:\n%s", ev)
	}
}

func TestGenerate_RepeatRules(t *testing.T) {
	cal := icalFor(t,
		events.Event{ID: "1", Name: "Once", Date: "2026-12-01", Reminders: []string{"1d"}},
		events.Event{ID: "2", Name: "Bday", Date: "1990-03-15", Repeat: events.Yearly, Reminders: []string{"1d"}},
		events.Event{ID: "3", Name: "Quarterly", Date: "2026-01-15", Repeat: events.Monthly, Every: 3, Reminders: []string{"1d"}},
		events.Event{ID: "4", Name: "Rent", Date: "2026-01-31", Repeat: events.Monthly, Reminders: []string{"1d"}},
		events.Event{ID: "5", Name: "Leap", Date: "2024-02-29", Repeat: events.Yearly, Reminders: []string{"1d"}},
		events.Event{ID: "6", Name: "Course", Date: "2026-10-05", Repeat: events.Weekly, Until: "2026-12-14", Reminders: []string{"1d"}},
		events.Event{ID: "7", Name: "Standup", Date: "2026-10-05T09:30", Repeat: events.Daily, Until: "2026-10-30", Reminders: []string{"10m"}},
	)
	want := map[string]string{
		"Bday":      "RRULE:FREQ=YEARLY\r\n",
		"Quarterly": "RRULE:FREQ=MONTHLY;INTERVAL=3\r\n",
		"Rent":      "RRULE:FREQ=MONTHLY;BYMONTHDAY=28,29,30,31;BYSETPOS=-1\r\n",
		"Leap":      "RRULE:FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=28,29;BYSETPOS=-1\r\n",
		"Course":    "RRULE:FREQ=WEEKLY;UNTIL=20261214\r\n",
		// Timed + TZID → UNTIL in UTC at the end of the until day (23:59:59 EDT = 03:59:59Z next day).
		"Standup": "RRULE:FREQ=DAILY;UNTIL=20261031T035959Z\r\n",
	}
	for name, rule := range want {
		if ev := vevent(t, cal, name); !strings.Contains(ev, rule) {
			t.Errorf("%s: want %q in\n%s", name, rule, ev)
		}
	}
	if strings.Contains(vevent(t, cal, "Once"), "RRULE") {
		t.Error("one-time event has an RRULE")
	}
	if ev := vevent(t, cal, "Bday"); !strings.Contains(ev, "DTSTART;VALUE=DATE:19900315") {
		t.Errorf("yearly should start on its first date:\n%s", ev)
	}
}

func TestGenerate_VAlarmsMatchOffsets(t *testing.T) {
	cal := icalFor(t, events.Event{ID: "f", Name: "Flight", Date: "2026-12-20T06:30", Reminders: []string{"30m", "12h", "1d", "7d", "0d", "90m"}})
	ev := vevent(t, cal, "Flight")
	for _, want := range []string{"TRIGGER:-PT30M", "TRIGGER:-PT12H", "TRIGGER:-P1D", "TRIGGER:-P7D", "TRIGGER:PT0S", "TRIGGER:-PT1H30M"} {
		if !strings.Contains(ev, want+"\r\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Count(ev, "BEGIN:VALARM") != 6 {
		t.Error("expected 6 VALARMs")
	}
}

func TestGenerate_AllDayVAlarmsAtNotifyTime(t *testing.T) {
	cal := icalFor(t,
		events.Event{ID: "a", Name: "Insurance", Date: "2026-12-01", Reminders: []string{"0d", "1d", "7d"}},
		events.Event{ID: "b", Name: "Early", Date: "2026-12-01", Reminders: []string{"2d"}, NotifyTime: "07:30"},
	)
	// All-day events start at 00:00, and their reminders arrive at the notify time.
	ins := vevent(t, cal, "Insurance")
	for _, want := range []string{"TRIGGER:PT9H", "TRIGGER:-PT15H", "TRIGGER:-P6DT15H"} {
		if !strings.Contains(ins, want+"\r\n") {
			t.Errorf("Insurance missing %s:\n%s", want, ins)
		}
	}
	if early := vevent(t, cal, "Early"); !strings.Contains(early, "TRIGGER:-P1DT16H30M\r\n") {
		t.Errorf("per-event notify time not used:\n%s", early)
	}
	// Each alarm has its own UID, so calendar apps don't merge them.
	for _, uid := range []string{"UID:a-0d@gonotifier", "UID:a-1d@gonotifier", "UID:a-7d@gonotifier"} {
		if !strings.Contains(ins, uid+"\r\n") {
			t.Errorf("missing alarm %s", uid)
		}
	}
}
