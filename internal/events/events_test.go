package events_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/events"
)

var loc = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}()

var nineAM = events.TimeOnly{Hour: 9}

func boolPtr(b bool) *bool { return &b }

func at(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, loc) }

// firstN collects the first n occurrences at or after from.
func firstN(e events.Event, from time.Time, n int) []time.Time {
	var out []time.Time
	for t := range e.Occurrences(from, loc) {
		out = append(out, t)
		if len(out) == n {
			break
		}
	}
	return out
}

func dates(ts []time.Time, layout string) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Format(layout))
	}
	return out
}

func TestParseDate(t *testing.T) {
	if got, err := events.ParseDate("2026-12-01", loc); err != nil || !got.Equal(at(2026, 12, 1, 0, 0)) {
		t.Errorf("date-only: %v %v", got, err)
	}
	if got, err := events.ParseDate("2026-11-15T10:00", loc); err != nil || !got.Equal(at(2026, 11, 15, 10, 0)) {
		t.Errorf("datetime: %v %v", got, err)
	}
	for _, bad := range []string{"2026-02-30", "2026-1-1", "2026-11-15 10:00", "03-15", "tomorrow", ""} {
		if _, err := events.ParseDate(bad, loc); err == nil {
			t.Errorf("ParseDate(%q) should fail", bad)
		}
	}
}

func TestParseOffset(t *testing.T) {
	cases := []struct {
		in   string
		dur  time.Duration
		unit string
	}{
		{"5d", 5 * 24 * time.Hour, "d"}, {"0d", 0, "d"}, {"12h", 12 * time.Hour, "h"}, {"30m", 30 * time.Minute, "m"},
	}
	for _, c := range cases {
		dur, unit, err := events.ParseOffset(c.in)
		if err != nil || dur != c.dur || unit != c.unit {
			t.Errorf("ParseOffset(%q) = %v, %q, %v", c.in, dur, unit, err)
		}
	}
	for _, bad := range []string{"5x", "d", "-1d", "1.5h", ""} {
		if _, _, err := events.ParseOffset(bad); err == nil {
			t.Errorf("ParseOffset(%q) should fail", bad)
		}
	}
}

func TestFireTime(t *testing.T) {
	timed := events.Event{Date: "2026-11-15T10:00"}
	occ := at(2026, 11, 15, 10, 0)
	allDay := events.Event{Date: "2026-12-01"}
	allDayOcc := at(2026, 12, 1, 0, 0)
	cases := []struct {
		e    events.Event
		off  string
		occ  time.Time
		want time.Time
	}{
		// At a time: exactly that long before, like calendar apps.
		{timed, "1d", occ, at(2026, 11, 14, 10, 0)},
		{timed, "12h", occ, at(2026, 11, 14, 22, 0)},
		{timed, "30m", occ, at(2026, 11, 15, 9, 30)},
		{timed, "0m", occ, occ}, // at the time
		// All-day: N days before, at the notify time.
		{allDay, "1d", allDayOcc, at(2026, 11, 30, 9, 0)},
		{allDay, "0d", allDayOcc, at(2026, 12, 1, 9, 0)}, // on the day
		{events.Event{Date: "2026-12-01", NotifyTime: "07:45"}, "3d", allDayOcc, at(2026, 11, 28, 7, 45)},
	}
	for _, c := range cases {
		got, err := events.FireTime(c.e, c.off, c.occ, nineAM)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("FireTime(%s @ %v) = %v, %v; want %v", c.off, c.occ, got, err, c.want)
		}
	}
}

func TestOccurrences_OneTime(t *testing.T) {
	e := events.Event{Date: "2026-11-15T10:00"}
	if got := firstN(e, at(2026, 10, 1, 0, 0), 5); len(got) != 1 || !got[0].Equal(at(2026, 11, 15, 10, 0)) {
		t.Errorf("before: %v", got)
	}
	if got := firstN(e, at(2026, 11, 16, 0, 0), 5); len(got) != 0 {
		t.Errorf("after: %v", got)
	}
}

func TestOccurrences_DailyWeekly(t *testing.T) {
	daily := events.Event{Date: "2026-10-01T08:00", Repeat: events.Daily, Every: 2}
	if got := dates(firstN(daily, at(2026, 10, 2, 0, 0), 3), "01-02 15:04"); !slices.Equal(got, []string{"10-03 08:00", "10-05 08:00", "10-07 08:00"}) {
		t.Errorf("every 2 days: %v", got)
	}
	weekly := events.Event{Date: "2026-10-05", Repeat: events.Weekly}
	if got := dates(firstN(weekly, at(2026, 10, 6, 0, 0), 2), "01-02"); !slices.Equal(got, []string{"10-12", "10-19"}) {
		t.Errorf("weekly: %v", got)
	}
}

func TestOccurrences_MonthlyClampsWithoutDrift(t *testing.T) {
	e := events.Event{Date: "2026-01-31", Repeat: events.Monthly}
	got := dates(firstN(e, at(2026, 1, 1, 0, 0), 5), "01-02")
	if want := []string{"01-31", "02-28", "03-31", "04-30", "05-31"}; !slices.Equal(got, want) {
		t.Errorf("monthly on 31st = %v, want %v", got, want)
	}
	quarterly := events.Event{Date: "2026-01-15", Repeat: events.Monthly, Every: 3}
	if got := dates(firstN(quarterly, at(2026, 2, 1, 0, 0), 3), "2006-01-02"); !slices.Equal(got, []string{"2026-04-15", "2026-07-15", "2026-10-15"}) {
		t.Errorf("every 3 months = %v", got)
	}
}

func TestOccurrences_YearlyLeapDay(t *testing.T) {
	e := events.Event{Date: "2024-02-29", Repeat: events.Yearly}
	got := dates(firstN(e, at(2025, 1, 1, 0, 0), 4), "2006-01-02")
	if want := []string{"2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29"}; !slices.Equal(got, want) {
		t.Errorf("Feb 29 yearly = %v, want %v", got, want)
	}
}

func TestOccurrences_Until(t *testing.T) {
	e := events.Event{Date: "2026-10-01", Repeat: events.Weekly, Until: "2026-10-22"}
	got := dates(firstN(e, at(2026, 10, 1, 0, 0), 10), "01-02")
	if want := []string{"10-01", "10-08", "10-15", "10-22"}; !slices.Equal(got, want) {
		t.Errorf("until (inclusive) = %v, want %v", got, want)
	}
}

func TestOccurrences_OldStartIsFast(t *testing.T) {
	e := events.Event{Date: "1990-03-15", Repeat: events.Daily}
	start := time.Now()
	got := firstN(e, at(2026, 10, 2, 12, 0), 1)
	if len(got) != 1 || got[0].Format("2006-01-02") != "2026-10-03" {
		t.Errorf("got %v", got)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("occurrence search took %v; should be fast even for old start dates", time.Since(start))
	}
}

func TestOccurrences_KeepsWallClockAcrossDST(t *testing.T) {
	e := events.Event{Date: "2026-10-31T08:00", Repeat: events.Daily} // US DST ends Nov 1
	for _, occ := range firstN(e, at(2026, 10, 31, 0, 0), 3) {
		if occ.Hour() != 8 {
			t.Errorf("%v: want 08:00 local", occ)
		}
	}
}

func TestNextOccurrence_AllDayTodayStillCounts(t *testing.T) {
	e := events.Event{Date: "2026-10-02"}
	if next, ok := e.NextOccurrence(at(2026, 10, 2, 15, 0), loc); !ok || next.Day() != 2 {
		t.Errorf("all-day today: %v %v", next, ok)
	}
	timed := events.Event{Date: "2026-10-02T09:00"}
	if _, ok := timed.NextOccurrence(at(2026, 10, 2, 15, 0), loc); ok {
		t.Error("timed event earlier today should be finished")
	}
}

func TestNormalize(t *testing.T) {
	e := events.Event{
		Name: "  Rent  ", Date: "2026-11-01T10:00:00", Repeat: "Monthly", Every: 1, Until: " 2027-12-31 ",
		Reminders: []string{"1D", "30m", "1d", "3d"}, Priority: "Default", AutoRemove: boolPtr(true),
	}
	events.Normalize(&e)
	want := events.Event{
		Name: "Rent", Date: "2026-11-01T10:00", Repeat: events.Monthly, Until: "2027-12-31",
		Reminders: []string{"30m", "1d", "3d"},
	}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("Normalize =\n%+v\nwant\n%+v", e, want)
	}

	once := events.Event{Date: "2026-11-01", Repeat: "never", Every: 5, Until: "2027-01-01"}
	events.Normalize(&once)
	if once.Repeat != "" || once.Every != 0 || once.Until != "" {
		t.Errorf("one-time event kept repeat fields: %+v", once)
	}
}

func TestValidate(t *testing.T) {
	ok := events.Event{Name: "A", Date: "2026-12-01", Reminders: []string{"1d"}}
	if errs := events.Validate(ok, nil, ""); len(errs) != 0 {
		t.Fatalf("valid event rejected: %v", errs)
	}
	cases := map[string]events.Event{
		"missing name":      {Date: "2026-12-01", Reminders: []string{"1d"}},
		"long name":         {Name: strings.Repeat("a", 101), Date: "2026-12-01", Reminders: []string{"1d"}},
		"bad date":          {Name: "A", Date: "2026-13-01", Reminders: []string{"1d"}},
		"no reminders":      {Name: "A", Date: "2026-12-01"},
		"bad reminder":      {Name: "A", Date: "2026-12-01", Reminders: []string{"5x"}},
		"bad repeat":        {Name: "A", Date: "2026-12-01", Repeat: "hourly", Reminders: []string{"1d"}},
		"hours on all-day":  {Name: "A", Date: "2026-12-01", Reminders: []string{"2h"}},
		"until before date": {Name: "A", Date: "2026-12-01", Repeat: events.Weekly, Until: "2026-11-01", Reminders: []string{"1d"}},
		"bad until":         {Name: "A", Date: "2026-12-01", Repeat: events.Weekly, Until: "soon", Reminders: []string{"1d"}},
		"lead ≥ period":     {Name: "A", Date: "2026-12-01", Repeat: events.Daily, Reminders: []string{"1d"}},
		"lead ≥ weeks":      {Name: "A", Date: "2026-12-01", Repeat: events.Weekly, Every: 2, Reminders: []string{"14d"}},
		"bad notify time":   {Name: "A", Date: "2026-12-01", Reminders: []string{"1d"}, NotifyTime: "9am"},
		"bad topic":         {Name: "A", Date: "2026-12-01", Reminders: []string{"1d"}, Topic: "a/b"},
		"bad priority":      {Name: "A", Date: "2026-12-01", Reminders: []string{"1d"}, Priority: "meh"},
	}
	for name, e := range cases {
		if errs := events.Validate(e, nil, ""); len(errs) == 0 {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	// Shorter-than-period reminders on repeats are fine.
	daily := events.Event{Name: "Pills", Date: "2026-12-01T08:00", Repeat: events.Daily, Reminders: []string{"0m", "30m"}}
	if errs := events.Validate(daily, nil, ""); len(errs) != 0 {
		t.Errorf("daily with short reminders rejected: %v", errs)
	}
	// Duplicates by name+date, except the event itself.
	existing := []events.Event{{ID: "x1", Name: "A", Date: "2026-12-01"}}
	if errs := events.Validate(ok, existing, ""); len(errs) != 1 {
		t.Errorf("duplicate not detected: %v", errs)
	}
	if errs := events.Validate(ok, existing, "x1"); len(errs) != 0 {
		t.Errorf("self counted as duplicate: %v", errs)
	}
}

func TestDescribeRepeat(t *testing.T) {
	cases := map[string]events.Event{
		"":                         {},
		"Monthly":                  {Repeat: events.Monthly},
		"Every 3 months":           {Repeat: events.Monthly, Every: 3},
		"Daily until Dec 31, 2027": {Repeat: events.Daily, Until: "2027-12-31"},
		"Every 2 years":            {Repeat: events.Yearly, Every: 2},
	}
	for want, e := range cases {
		if got := e.DescribeRepeat(); got != want {
			t.Errorf("DescribeRepeat(%+v) = %q, want %q", e, got, want)
		}
	}
}

func TestDecodeFormats(t *testing.T) {
	inputs := map[string]string{
		"document":        `{"events":[{"name":"A","date":"2026-12-01","reminders":["1d"]}]}`,
		"list":            `[{"name":"A","date":"2026-12-01","reminders":["1d"]}]`,
		"api list (next)": `[{"id":"a1","name":"A","date":"2026-12-01","reminders":["1d"],"next":"2026-12-01T00:00:00Z"}]`,
	}
	for name, in := range inputs {
		evs, err := events.Decode([]byte(in))
		if err != nil || len(evs) != 1 || evs[0].Name != "A" || evs[0].Date != "2026-12-01" {
			t.Errorf("%s: %+v %v", name, evs, err)
		}
	}
	if evs, err := events.Decode([]byte(`{"events":[]}`)); err != nil || len(evs) != 0 {
		t.Errorf("empty document: %v %v", evs, err)
	}
	for _, bad := range []string{"", "events:\n  - name: A", `{"events": "nope"}`, `{"name":"A"}`, `[{"name":`} {
		if _, err := events.Decode([]byte(bad)); err == nil {
			t.Errorf("Decode(%q) should fail", bad)
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	in := []events.Event{
		{ID: "a1", Name: "Rent", Date: "2026-11-01", Repeat: events.Monthly, Every: 2, Until: "2027-12-31", Reminders: []string{"1d"}},
		{ID: "b2", Name: "Flight", Date: "2026-12-20T06:30", Reminders: []string{"30m", "1d"}, AutoRemove: boolPtr(false), Topic: "travel", Priority: "high", Tags: "plane"},
	}
	data, err := events.Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := events.Decode(data)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Errorf("round trip:\n%+v\n%+v (%v)\n%s", in, out, err, data)
	}
	if data, _ := events.Encode(nil); !strings.Contains(string(data), `"events": []`) {
		t.Errorf("empty export = %s", data)
	}
}

func TestNormalize_ZeroReminders(t *testing.T) {
	timed := events.Event{Date: "2026-12-01T08:00", Reminders: []string{"0d", "0h", "30m"}, NotifyTime: "07:00"}
	events.Normalize(&timed)
	if strings.Join(timed.Reminders, ",") != "0m,30m" || timed.NotifyTime != "" {
		t.Errorf("timed: reminders %v, notify time %q", timed.Reminders, timed.NotifyTime)
	}
	allDay := events.Event{Date: "2026-12-01", Reminders: []string{"0m", "1d"}}
	events.Normalize(&allDay)
	if strings.Join(allDay.Reminders, ",") != "0d,1d" {
		t.Errorf("all-day: reminders %v", allDay.Reminders)
	}
}
