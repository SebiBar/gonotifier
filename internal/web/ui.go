package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/notify"
	"github.com/sebibar/gonotifier/internal/store"
)

type option struct{ Value, Label string }

// chip is a reminder choice in the form; Minutes is how long before the event it is.
type chip struct {
	Value, Label string
	Minutes      int
}

var (
	// Like calendar apps: events at a time get exact durations, all-day events get days
	// (sent at the notify time).
	timedChips = []chip{
		{"0m", "At time", 0}, {"10m", "10 min", 10}, {"30m", "30 min", 30}, {"1h", "1 hour", 60},
		{"12h", "12 hours", 720}, {"1d", "1 day", 1440}, {"2d", "2 days", 2880}, {"7d", "1 week", 10080},
	}
	allDayChips = []chip{
		{"0d", "On the day", 0}, {"1d", "1 day", 1440}, {"2d", "2 days", 2880}, {"3d", "3 days", 4320},
		{"7d", "1 week", 10080}, {"14d", "2 weeks", 20160}, {"30d", "30 days", 43200},
	}
	repeatOptions = []option{
		{events.Once, "Once"}, {events.Daily, "Daily"}, {events.Weekly, "Weekly"},
		{events.Monthly, "Monthly"}, {events.Yearly, "Yearly"},
	}
	priorityOptions = []option{{"low", "Low"}, {"default", "Normal"}, {"high", "High"}, {"urgent", "Urgent"}}
)

type formData struct {
	Mode              string // "new" or "edit"
	ID                string // event ID (edit)
	Event             events.Event
	Date              string // YYYY-MM-DD
	Time              string // HH:MM, "" = all day
	Repeat            string
	Every             string
	Until             string
	Selected          []string // checked reminder chips
	CustomReminders   string   // reminders that aren't chips
	AutoRemove        bool
	Priority          string
	ShowMore          bool // open "More options" (when any of them is set)
	Errors            []string
	DefaultNotifyTime string
	DefaultTopic      string
}

func (fd formData) isSelected(offset string) bool { return slices.Contains(fd.Selected, offset) }

// alpineState is the form's Alpine.js x-data, JSON-encoded so values are always safely escaped.
func (fd formData) alpineState() string {
	state, _ := templ.JSONString(map[string]any{
		"repeat": fd.Repeat, "every": fd.Every, "until": fd.Until, "more": fd.ShowMore,
		"time": fd.Time, "notifyTime": fd.Event.NotifyTime, "defaultNotify": fd.DefaultNotifyTime,
	})
	return state // strings and a bool always encode
}

// fits is an Alpine.js expression: whether a reminder this many minutes before the event is
// shorter than the repeat interval. Longer ones are hidden (and rejected by events.Validate).
func fits(minutes int) string {
	return fmt.Sprintf("!repeat || %d < {daily: 1440, weekly: 10080, monthly: 40320, yearly: 525600}[repeat] * Math.max(1, +every || 1)", minutes)
}

// renderList returns the event list plus out-of-band refreshes of the count and "Upcoming".
func (s *server) renderList(w http.ResponseWriter, r *http.Request, highlight string) {
	p, err := s.page(userOf(r), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.Highlight = highlight
	render(w, r, http.StatusOK, EventList(p), ListOOB(p))
}

// renderForm returns the add/edit form and asks the page to open the dialog once it is swapped in,
// so the dialog never shows a loading state.
func renderForm(w http.ResponseWriter, r *http.Request, fd formData) {
	w.Header().Set("HX-Trigger-After-Swap", "open-dialog")
	render(w, r, http.StatusOK, Form(fd))
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	p, err := s.page(userOf(r), true)
	if err != nil {
		http.Error(w, "failed to load events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, Page(p))
}

func (s *server) newFormData(user string) formData {
	return formData{
		Mode: "new", AutoRemove: true, Priority: "default",
		DefaultNotifyTime: s.cfg.DefaultNotifyTime.String(),
		DefaultTopic:      notify.DefaultTopic(user),
	}
}

// fillForm populates the form view from an event.
func fillForm(fd *formData, e events.Event) {
	fd.Event = e
	fd.Date, fd.Time, _ = strings.Cut(e.Date, "T")
	fd.Repeat, fd.Until = e.Repeat, e.Until
	fd.Every = ""
	if e.Every > 1 {
		fd.Every = strconv.Itoa(e.Every)
	}
	fd.Selected = nil
	chips := allDayChips
	if e.HasTime() {
		chips = timedChips
	}
	var custom []string
	for _, rem := range e.Reminders {
		if slices.ContainsFunc(chips, func(c chip) bool { return c.Value == rem }) {
			fd.Selected = append(fd.Selected, rem)
		} else {
			custom = append(custom, rem)
		}
	}
	fd.CustomReminders = strings.Join(custom, ", ")
	fd.AutoRemove = e.ShouldAutoRemove()
	fd.Priority = e.Priority
	if fd.Priority == "" {
		fd.Priority = "default"
	}
	fd.ShowMore = (e.NotifyTime != "" && !e.HasTime()) || e.Topic != "" || e.Tags != "" || fd.Priority != "default" || !fd.AutoRemove
}

func (s *server) newForm(w http.ResponseWriter, r *http.Request) {
	renderForm(w, r, s.newFormData(userOf(r)))
}

func (s *server) editForm(w http.ResponseWriter, r *http.Request) {
	e, err := s.store.GetEvent(userOf(r), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fd := s.newFormData(userOf(r))
	fd.Mode, fd.ID = "edit", e.ID
	fillForm(&fd, e)
	renderForm(w, r, fd)
}

// eventFromForm builds an Event from the add/edit form submission.
func eventFromForm(r *http.Request) events.Event {
	e := events.Event{
		Name:       r.FormValue("name"),
		Date:       strings.TrimSpace(r.FormValue("date")),
		Repeat:     r.FormValue("repeat"),
		NotifyTime: r.FormValue("notify_time"),
		Topic:      r.FormValue("topic"),
		Priority:   r.FormValue("priority"),
		Tags:       r.FormValue("tags"),
	}
	if t := strings.TrimSpace(r.FormValue("time")); t != "" && e.Date != "" {
		if len(t) > 5 {
			t = t[:5] // drop seconds some browsers append
		}
		e.Date += "T" + t
	}
	if e.Repeat != events.Once {
		e.Every, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("every")))
		e.Until = r.FormValue("until")
	}
	// Chips arrive as repeated "reminders" values; custom ones as free text.
	e.Reminders = events.SplitReminders(strings.Join(r.Form["reminders"], ",") + "," + r.FormValue("custom_reminders"))
	if r.FormValue("auto_remove") == "" {
		f := false
		e.AutoRemove = &f
	}
	return e
}

func (s *server) submitForm(w http.ResponseWriter, r *http.Request, editID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user := userOf(r)
	e := eventFromForm(r)
	var saved events.Event
	err := s.checkTiming(user, editID, e)
	switch {
	case err != nil:
	case editID == "":
		saved, err = s.store.CreateEvent(user, e)
	default:
		saved, err = s.store.UpdateEvent(user, editID, e)
	}

	var verrs events.ValidationError
	switch {
	case errors.As(err, &verrs):
		fd := s.newFormData(user)
		if editID != "" {
			fd.Mode, fd.ID = "edit", editID
		}
		fillForm(&fd, e) // shows exactly what the user submitted
		fd.Errors = verrs
		// Re-render the form inside the dialog instead of swapping the event list.
		w.Header().Set("HX-Retarget", "#dialog-content")
		w.Header().Set("HX-Reswap", "innerHTML")
		render(w, r, http.StatusOK, Form(fd))
	case errors.Is(err, store.ErrNotFound):
		w.Header().Set("HX-Reswap", "none")
		setTrigger(w, true, "Event no longer exists", "error")
		w.WriteHeader(http.StatusOK)
	case err != nil:
		slog.Error("save event", "err", err)
		w.Header().Set("HX-Reswap", "none")
		setTrigger(w, false, "Failed to save: "+err.Error(), "error")
		w.WriteHeader(http.StatusOK)
	default:
		s.sched.Wake()
		msg := "Event added"
		if editID != "" {
			msg = "Changes saved"
		}
		setTrigger(w, true, msg, "success")
		s.renderList(w, r, saved.ID)
	}
}

func (s *server) createEvent(w http.ResponseWriter, r *http.Request) { s.submitForm(w, r, "") }

func (s *server) updateEvent(w http.ResponseWriter, r *http.Request) {
	s.submitForm(w, r, r.PathValue("id"))
}

func (s *server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteEvent(userOf(r), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Already gone — let the row fade out anyway.
		setTrigger(w, false, "Event already deleted", "info")
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		slog.Error("delete event", "err", err)
		w.Header().Set("HX-Reswap", "none")
		setTrigger(w, false, "Failed to delete: "+err.Error(), "error")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.sched.Wake()
	setTrigger(w, false, "Event deleted", "success")
	// Empty main body removes the row; the count and "Upcoming" refresh out-of-band.
	p, err := s.page(userOf(r), false)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	render(w, r, http.StatusOK, ListOOB(p))
}

// resetFeed gives the user a new calendar feed URL; the old one stops working.
func (s *server) resetFeed(w http.ResponseWriter, r *http.Request) {
	token, err := s.store.ResetFeedToken(userOf(r))
	if err != nil {
		slog.Error("reset feed", "err", err)
		w.Header().Set("HX-Reswap", "none")
		setTrigger(w, false, "Failed to reset the link: "+err.Error(), "error")
		w.WriteHeader(http.StatusOK)
		return
	}
	setTrigger(w, false, "New link created: copy it into your calendar app", "success")
	render(w, r, http.StatusOK, FeedURL(s.feedURL(token)))
}
