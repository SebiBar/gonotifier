package web

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/store"
)

type EventView struct {
	ID          string
	Name        string
	DateDisplay string // next occurrence (or the date, once finished)
	TypeIcon    string // sprite icon name
	TypeLabel   string // "Monthly", "Every 3 days", "All day", "Timed"
	Repeating   bool
	Reminders   []string
	KeptAfter   bool   // won't be auto-removed once it ends
	Next        string // "in 3 days", "today", "passed"
	Soon        bool   // within ~2 days
	Priority    string
	next        time.Time
}

type UpcomingView struct {
	Month        string // "OCT"
	Day          string // "5"
	Weekday      string // "Monday"
	Detail       string // "10:00" / "All day" / "Monthly"
	EventName    string
	RelativeTime string
	Soon         bool
	at           time.Time
}

type HistoryView struct {
	SentAt    string
	EventName string
	Message   string
}

type pageData struct {
	Username  string
	Upcoming  []UpcomingView
	Events    []EventView
	History   []HistoryView
	Highlight string // ID of the event that was just added/edited
	FeedURL   string // the user's secret calendar feed URL
}

func isSoon(t, now time.Time) bool { return t.Sub(now) < 48*time.Hour }

func relativeTime(t, now time.Time, hasTime bool) string {
	d := t.Sub(now)
	if hasTime && d >= 0 && d < 24*time.Hour {
		switch {
		case d < time.Minute:
			return "now"
		case d < time.Hour:
			return fmt.Sprintf("in %d min", int(d/time.Minute))
		default:
			return "in " + events.HumanOffset(d.Truncate(time.Hour), "h")
		}
	}
	days := calendarDays(now, t)
	switch {
	case days < 0:
		return "passed"
	case days == 0:
		return "today"
	case days == 1:
		return "tomorrow"
	}
	return fmt.Sprintf("in %d days", days)
}

func calendarDays(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return int(time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)) / (24 * time.Hour))
}

// formatDate renders an occurrence; yearly events omit the year (it's the same every year).
func formatDate(e events.Event, t time.Time) string {
	layout := "Jan 2, 2006"
	if e.Repeat == events.Yearly {
		layout = "Jan 2"
	}
	if e.HasTime() {
		layout += " · 15:04"
	}
	return t.Format(layout)
}

// reminderLabels shows reminders as stored ("30m", "1d"), except the ones without an offset.
func reminderLabels(e events.Event) []string {
	out := make([]string, 0, len(e.Reminders))
	for _, r := range e.Reminders {
		switch {
		case r == "0m":
			r = "at time"
		case r == "0d":
			r = "on the day"
		}
		out = append(out, r)
	}
	return out
}

func typeOf(e events.Event) (icon, label string) {
	switch {
	case e.IsRepeating():
		return "repeat", e.DescribeRepeat()
	case e.HasTime():
		return "clock", "Timed"
	}
	return "calendar", "All day"
}

func (s *server) buildEventViews(evs []events.Event, now time.Time) ([]EventView, []UpcomingView) {
	loc := s.cfg.TZ
	var list []EventView
	var upcoming []UpcomingView
	horizon := now.AddDate(0, 0, 30)
	for _, e := range evs {
		icon, label := typeOf(e)
		v := EventView{
			ID: e.ID, Name: e.Name, TypeIcon: icon, TypeLabel: label, Repeating: e.IsRepeating(),
			Reminders: reminderLabels(e), Priority: e.Priority, Next: "passed",
			KeptAfter: !e.ShouldAutoRemove() && (!e.IsRepeating() || e.Until != ""),
		}
		if start, err := e.Start(loc); err == nil {
			v.DateDisplay = formatDate(e, start)
		}
		if next, ok := e.NextOccurrence(now, loc); ok {
			v.next = next
			v.DateDisplay = formatDate(e, next)
			v.Next = relativeTime(next, now, e.HasTime())
			v.Soon = isSoon(next, now)
			if next.Before(horizon) {
				detail := label
				if e.HasTime() {
					detail = next.Format("15:04")
					if e.IsRepeating() {
						detail += " · " + label
					}
				}
				upcoming = append(upcoming, UpcomingView{
					Month: strings.ToUpper(next.Format("Jan")), Day: next.Format("2"), Weekday: next.Format("Monday"),
					Detail: detail, EventName: e.Name, RelativeTime: v.Next, Soon: v.Soon, at: next,
				})
			}
		}
		list = append(list, v)
	}
	// Soonest first; finished events sink to the bottom.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].next, list[j].next
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		return a.Before(b)
	})
	sort.SliceStable(upcoming, func(i, j int) bool { return upcoming[i].at.Before(upcoming[j].at) })
	return list, upcoming
}

func (s *server) buildPage(u store.User, evs []events.Event, withHistory bool) pageData {
	now := clock.Now().In(s.cfg.TZ)
	p := pageData{Username: u.Username, FeedURL: s.feedURL(u.FeedToken)}
	p.Events, p.Upcoming = s.buildEventViews(evs, now)
	if withHistory {
		recs, err := s.store.RecentHistory(u.Username, 20)
		if err != nil {
			slog.Error("load history", "err", err)
		}
		for _, r := range recs {
			p.History = append(p.History, HistoryView{
				SentAt: r.SentAt.In(s.cfg.TZ).Format("Jan 2 15:04"), EventName: r.EventName, Message: r.Message,
			})
		}
	}
	return p
}

// page loads the user's events and builds the view.
func (s *server) page(user string, withHistory bool) (pageData, error) {
	u, err := s.store.GetUser(user)
	if err != nil {
		return pageData{}, err
	}
	evs, err := s.store.ListEvents(user)
	if err != nil {
		return pageData{}, err
	}
	return s.buildPage(u, evs, withHistory), nil
}
