package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/store"
)

// The JSON API has no authentication — expose it only on an internal network.

const maxImportBytes = 4 << 20

// apiEvent is an event plus its computed next occurrence.
type apiEvent struct {
	events.Event
	Next string `json:"next,omitempty"` // RFC 3339; empty once finished
}

func (s *server) toAPI(e events.Event) apiEvent {
	out := apiEvent{Event: e}
	if next, ok := e.NextOccurrence(clock.Now().In(s.cfg.TZ), s.cfg.TZ); ok {
		out.Next = next.Format(time.RFC3339)
	}
	return out
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// apiSaveError maps store errors to responses.
func apiSaveError(w http.ResponseWriter, err error) {
	var verrs events.ValidationError
	switch {
	case errors.As(err, &verrs):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "validation failed", "errors": []string(verrs)})
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "event not found")
	default:
		slog.Error("api", "err", err)
		apiError(w, http.StatusInternalServerError, err.Error())
	}
}

func decodeEvent(w http.ResponseWriter, r *http.Request) (events.Event, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var in apiEvent // accepts the read-only "next" field, so GET → edit → PUT round-trips
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return in.Event, false
	}
	return in.Event, true
}

func (s *server) apiListEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.store.ListEvents()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]apiEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, s.toAPI(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) apiGetEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.store.GetEvent(r.PathValue("id"))
	if err != nil {
		apiSaveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toAPI(e))
}

func (s *server) apiCreateEvent(w http.ResponseWriter, r *http.Request) {
	e, ok := decodeEvent(w, r)
	if !ok {
		return
	}
	e.ID = "" // IDs are assigned by the server
	saved, err := s.store.CreateEvent(e)
	if err != nil {
		apiSaveError(w, err)
		return
	}
	s.sched.Wake()
	writeJSON(w, http.StatusCreated, s.toAPI(saved))
}

func (s *server) apiUpdateEvent(w http.ResponseWriter, r *http.Request) {
	e, ok := decodeEvent(w, r)
	if !ok {
		return
	}
	saved, err := s.store.UpdateEvent(r.PathValue("id"), e)
	if err != nil {
		apiSaveError(w, err)
		return
	}
	s.sched.Wake()
	writeJSON(w, http.StatusOK, s.toAPI(saved))
}

func (s *server) apiDeleteEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteEvent(r.PathValue("id")); err != nil {
		apiSaveError(w, err)
		return
	}
	s.sched.Wake()
	w.WriteHeader(http.StatusNoContent)
}

// apiExport returns all events as {"events": [...]}, ready to be imported again.
func (s *server) apiExport(w http.ResponseWriter, r *http.Request) {
	evs, err := s.store.ListEvents()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	data, err := events.Encode(evs)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// apiImport adds or updates events from a JSON document (see events.Decode).
// Events with an existing id are updated; others are created. ?mode=replace also
// deletes events that aren't in the document. All-or-nothing: any invalid event
// rejects the whole import.
func (s *server) apiImport(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		apiError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	evs, err := events.Decode(data)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	replace := r.URL.Query().Get("mode") == "replace"
	res, err := s.store.Import(evs, replace)
	if err != nil {
		apiSaveError(w, err)
		return
	}
	s.sched.Wake()
	writeJSON(w, http.StatusOK, res)
}
