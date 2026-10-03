// Package web serves the htmx UI, the JSON API, the iCal feed and the health check.
//
// Layout:
//
//	web.go       routing, static assets, rendering helpers, feed + health
//	ui.go        htmx dashboard and add/edit/delete handlers
//	views.go     view models the components render
//	api.go       JSON API (CRUD, import/export) for scripts, other containers and AI tools
//	*.templ      templ components (layout, event list, form); *_templ.go is generated
//	static/      app.css, favicon, vendor/ (pinned third-party assets)
//
// After editing a .templ file, run: go tool templ generate
package web

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"

	"github.com/sebibar/gonotifier/internal/config"
	"github.com/sebibar/gonotifier/internal/ical"
	"github.com/sebibar/gonotifier/internal/store"
)

//go:embed static
var staticFS embed.FS

// assetVersion busts browser caches of /static/* whenever the embedded files change.
var assetVersion = func() string {
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(path)
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// asset returns a cache-busting URL for a file in static/.
func asset(path string) string { return "/static/" + path + "?v=" + assetVersion }

const maxBodyBytes = 64 << 10

// Scheduler is the part of the scheduler the web layer uses.
type Scheduler interface {
	Wake()                        // re-plan after events change
	LastCheck() (time.Time, bool) // for /health
	Next() (time.Time, bool)      // next scheduled reminder
}

type server struct {
	cfg   *config.Config
	store *store.Store
	sched Scheduler
}

// Register sets up all routes (UI, API, feed, health, static files) on the mux.
func Register(mux *http.ServeMux, cfg *config.Config, st *store.Store, sched Scheduler) {
	s := &server{cfg: cfg, store: st, sched: sched}

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /events/new", s.newForm)
	mux.HandleFunc("GET /events/{id}/edit", s.editForm)
	mux.HandleFunc("POST /events", s.createEvent)
	mux.HandleFunc("PUT /events/{id}", s.updateEvent)
	mux.HandleFunc("DELETE /events/{id}", s.deleteEvent)

	mux.HandleFunc("GET /api/events", s.apiListEvents)
	mux.HandleFunc("POST /api/events", s.apiCreateEvent)
	mux.HandleFunc("GET /api/events/{id}", s.apiGetEvent)
	mux.HandleFunc("PUT /api/events/{id}", s.apiUpdateEvent)
	mux.HandleFunc("DELETE /api/events/{id}", s.apiDeleteEvent)
	mux.HandleFunc("GET /api/export", s.apiExport)
	mux.HandleFunc("POST /api/import", s.apiImport)
	mux.HandleFunc("GET /api/health", s.health)

	mux.HandleFunc("GET /feed.ics", s.feed)
	mux.HandleFunc("GET /feed/{file}", s.feed) // /feed/<FEED_TOKEN>.ics
	mux.HandleFunc("GET /health", s.health)

	static := http.FileServerFS(staticFS)
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // URL changes with content
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600") // e.g. fonts referenced from CSS
		}
		static.ServeHTTP(w, r)
	}))
}

// ---------- rendering helpers ----------

// render writes the components in order as one HTML response.
func render(w http.ResponseWriter, r *http.Request, status int, components ...templ.Component) {
	var buf bytes.Buffer
	for _, c := range components {
		if err := c.Render(r.Context(), &buf); err != nil {
			slog.Error("render", "err", err)
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// setTrigger sends htmx client events: a toast, and optionally closing the dialog.
func setTrigger(w http.ResponseWriter, closeDialog bool, message, kind string) {
	trig := map[string]any{"show-toast": map[string]string{"message": message, "type": kind}}
	if closeDialog {
		trig["close-dialog"] = true
	}
	b, _ := json.Marshal(trig)
	w.Header().Set("HX-Trigger", string(b))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ---------- feed + health ----------

// feed serves the iCal feed, but only at cfg.FeedPath(): with FEED_TOKEN set,
// /feed.ics and wrong tokens get a 404, so the URL itself is the secret.
func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(s.cfg.FeedPath())) != 1 {
		http.NotFound(w, r)
		return
	}
	evs, err := s.store.ListEvents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="gonotifier.ics"`)
	w.Write([]byte(ical.Generate(evs, s.cfg.TZ)))
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"status": "ok", "events": 0, "last_check": nil, "next_reminder": nil}
	status := http.StatusOK
	if evs, err := s.store.ListEvents(); err != nil {
		resp["status"], resp["error"] = "error", err.Error()
		status = http.StatusServiceUnavailable
	} else {
		resp["events"] = len(evs)
	}
	if t, ok := s.sched.LastCheck(); ok {
		resp["last_check"] = t.Format(time.RFC3339)
	}
	if t, ok := s.sched.Next(); ok {
		resp["next_reminder"] = t.In(s.cfg.TZ).Format(time.RFC3339)
	}
	writeJSON(w, status, resp)
}
