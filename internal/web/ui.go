package web

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/store"
)

type option struct{ Value, Label string }

var (
	chipOffsets = []option{
		{"30m", "30 min"}, {"1h", "1 hour"}, {"12h", "12 hours"}, {"1d", "1 day"}, {"2d", "2 days"},
		{"3d", "3 days"}, {"7d", "1 week"}, {"14d", "2 weeks"}, {"30d", "30 days"},
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
	state, _ := templ.JSONString(map[string]any{"repeat": fd.Repeat, "every": fd.Every, "until": fd.Until, "more": fd.ShowMore})
	return state // strings and a bool always encode
}

// renderList returns the event list plus out-of-band refreshes of the count and "Upcoming".
func (s *server) renderList(w http.ResponseWriter, r *http.Request, highlight string) {
	p, err := s.page(false)
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
	p, err := s.page(true)
	if err != nil {
		http.Error(w, "failed to load events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, Page(p))
}

func (s *server) newFormData() formData {
	return formData{
		Mode: "new", AutoRemove: true, Priority: "default",
		DefaultNotifyTime: s.cfg.DefaultNotifyTime.String(),
		DefaultTopic:      s.cfg.NtfyDefaultTopic,
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
	var custom []string
	for _, rem := range e.Reminders {
		if slices.ContainsFunc(chipOffsets, func(c option) bool { return c.Value == rem }) {
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
	fd.ShowMore = e.NotifyTime != "" || e.Topic != "" || e.Tags != "" || fd.Priority != "default" || !fd.AutoRemove
}

func (s *server) newForm(w http.ResponseWriter, r *http.Request) {
	renderForm(w, r, s.newFormData())
}

func (s *server) editForm(w http.ResponseWriter, r *http.Request) {
	e, err := s.store.GetEvent(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fd := s.newFormData()
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
	e := eventFromForm(r)
	var saved events.Event
	var err error
	if editID == "" {
		saved, err = s.store.CreateEvent(e)
	} else {
		saved, err = s.store.UpdateEvent(editID, e)
	}

	var verrs events.ValidationError
	switch {
	case errors.As(err, &verrs):
		fd := s.newFormData()
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
	err := s.store.DeleteEvent(r.PathValue("id"))
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
	p, err := s.page(false)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	render(w, r, http.StatusOK, ListOOB(p))
}
