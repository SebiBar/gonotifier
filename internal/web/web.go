// Package web serves the htmx UI, the JSON API, the iCal feed and the health check.
//
// Layout:
//
//	web.go       routing, static assets, rendering helpers, feed + health
//	auth.go      login (checked by ntfy), sessions, API authentication
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
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/sebibar/gonotifier/internal/config"
	"github.com/sebibar/gonotifier/internal/ical"
	"github.com/sebibar/gonotifier/internal/notify"
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
	ntfy  notify.Ntfy // checks logins
	auth  *auth
}

// Handler returns the whole web app: UI, API, feed, health and static files.
// Everything except the login page, the feed, health and static files needs a logged-in user.
func Handler(cfg *config.Config, st *store.Store, sched Scheduler) http.Handler {
	s := &server{cfg: cfg, store: st, sched: sched, ntfy: notify.Ntfy{URL: cfg.NtfyURL}, auth: newAuth()}
	mux := http.NewServeMux()
	ui, api := s.requireUser, s.requireAPIUser

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /{$}", ui(s.dashboard))
	mux.HandleFunc("GET /events/new", ui(s.newForm))
	mux.HandleFunc("GET /events/{id}/edit", ui(s.editForm))
	mux.HandleFunc("POST /events", ui(s.createEvent))
	mux.HandleFunc("PUT /events/{id}", ui(s.updateEvent))
	mux.HandleFunc("DELETE /events/{id}", ui(s.deleteEvent))
	mux.HandleFunc("POST /feed/reset", ui(s.resetFeed))

	mux.HandleFunc("GET /api/events", api(s.apiListEvents))
	mux.HandleFunc("POST /api/events", api(s.apiCreateEvent))
	mux.HandleFunc("GET /api/events/{id}", api(s.apiGetEvent))
	mux.HandleFunc("PUT /api/events/{id}", api(s.apiUpdateEvent))
	mux.HandleFunc("DELETE /api/events/{id}", api(s.apiDeleteEvent))
	mux.HandleFunc("GET /api/export", api(s.apiExport))
	mux.HandleFunc("POST /api/import", api(s.apiImport))
	mux.HandleFunc("GET /api/health", s.health)

	mux.HandleFunc("GET /feed/{file}", s.feed) // /feed/<user's feed token>.ics
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

	// Rejects form posts and API calls a browser makes on behalf of another website (CSRF).
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin") // the feed URL is a secret
		next.ServeHTTP(w, r)
	})
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

// feedURL is where a user's calendar feed is served (on FEED_URL if set, so the UI can show
// the public address). The URL itself is the secret: calendar apps fetch it without logging in.
func (s *server) feedURL(token string) string { return s.cfg.FeedURL + "/feed/" + token + ".ics" }

// feed serves the iCal feed of the user whose feed token is in the URL; anything else is a 404.
func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutSuffix(r.PathValue("file"), ".ics")
	if !ok || token == "" {
		http.NotFound(w, r)
		return
	}
	u, err := s.store.UserByFeedToken(token)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	evs, err := s.store.ListEvents(u.Username)
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
	if evs, err := s.store.AllEvents(); err != nil {
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
