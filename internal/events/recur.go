package events

import (
	"fmt"
	"iter"
	"time"
)

// Occurrences yields the event's occurrences at or after from, in order.
// One-time events yield at most one. Repeats stop after `until` (inclusive day).
//
// Monthly and yearly repeats are computed from the start date, clamping to the
// last day of shorter months: a repeat on the 31st falls on Apr 30, Feb 28, …,
// and a yearly Feb 29 falls on Feb 28 in non-leap years. Wall-clock time is kept
// across DST changes.
func (e Event) Occurrences(from time.Time, loc *time.Location) iter.Seq[time.Time] {
	return func(yield func(time.Time) bool) {
		start, err := e.Start(loc)
		if err != nil {
			return
		}
		if !e.IsRepeating() {
			if !start.Before(from) {
				yield(start)
			}
			return
		}
		var end time.Time // exclusive bound: the day after `until`
		if e.Until != "" {
			u, err := ParseDate(e.Until, loc)
			if err != nil {
				return
			}
			end = u.AddDate(0, 0, 1)
		}
		for k := 0; ; k++ {
			t := e.nth(start, k)
			if !end.IsZero() && !t.Before(end) {
				return
			}
			if t.Before(from) {
				continue
			}
			if !yield(t) {
				return
			}
		}
	}
}

// NextOccurrence returns the first occurrence that hasn't ended at now.
// All-day occurrences last the whole day, so today's still counts.
func (e Event) NextOccurrence(now time.Time, loc *time.Location) (time.Time, bool) {
	from := now
	if !e.HasTime() {
		y, m, d := now.In(loc).Date()
		from = time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	for t := range e.Occurrences(from, loc) {
		return t, true
	}
	return time.Time{}, false
}

// nth returns occurrence k (k ≥ 0) of a repeating event.
func (e Event) nth(start time.Time, k int) time.Time {
	n := k * e.Interval()
	y, m, d := start.Date()
	h, mi := start.Hour(), start.Minute()
	switch e.Repeat {
	case Daily:
		return time.Date(y, m, d+n, h, mi, 0, 0, start.Location())
	case Weekly:
		return time.Date(y, m, d+7*n, h, mi, 0, 0, start.Location())
	case Monthly:
		return addMonthsClamped(start, n)
	case Yearly:
		return addMonthsClamped(start, 12*n)
	}
	return start
}

func addMonthsClamped(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	total := int(m) - 1 + n
	ny, nm := y+total/12, time.Month(total%12+1)
	if last := daysIn(nm, ny); d > last {
		d = last
	}
	return time.Date(ny, nm, d, t.Hour(), t.Minute(), 0, 0, t.Location())
}

func daysIn(m time.Month, year int) int {
	return time.Date(year, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// periodDays is the shortest possible gap between occurrences, in days.
func (e Event) periodDays() int {
	switch e.Repeat {
	case Daily:
		return e.Interval()
	case Weekly:
		return 7 * e.Interval()
	case Monthly:
		return 28 * e.Interval()
	case Yearly:
		return 365 * e.Interval()
	}
	return 0
}

// DescribeRepeat renders the rule: "Daily", "Every 3 months", "Yearly until Dec 31, 2027".
func (e Event) DescribeRepeat() string {
	if !e.IsRepeating() {
		return ""
	}
	var s string
	if n := e.Interval(); n == 1 {
		s = map[string]string{Daily: "Daily", Weekly: "Weekly", Monthly: "Monthly", Yearly: "Yearly"}[e.Repeat]
	} else {
		unit := map[string]string{Daily: "days", Weekly: "weeks", Monthly: "months", Yearly: "years"}[e.Repeat]
		s = fmt.Sprintf("Every %d %s", n, unit)
	}
	if e.Until != "" {
		if u, err := ParseDate(e.Until, time.UTC); err == nil {
			s += " until " + u.Format("Jan 2, 2006")
		}
	}
	return s
}
