// Package testutil provides shared fixtures for package tests: a temp SQLite store
// seeded with events, a fake ntfy server and a frozen clock.
package testutil

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/config"
	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/store"
)

// Loc is the timezone all tests run in.
var Loc = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

// FixedNow is the frozen "current time": Fri 2 Oct 2026, 10:00 New York.
var FixedNow = time.Date(2026, 10, 2, 10, 0, 0, 0, Loc)

// Notification is one request received by the fake ntfy server.
type Notification struct {
	Topic, Title, Body, Priority, Tags, Auth string
}

// Recorder collects notifications sent to the fake ntfy server.
type Recorder struct {
	mu   sync.Mutex
	sent []Notification
}

// All returns a copy of everything received so far.
func (r *Recorder) All() []Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Notification(nil), r.sent...)
}

type Env struct {
	Cfg   *config.Config
	Store *store.Store
	Ntfy  *Recorder
}

// NewEnv freezes clock.Now at FixedNow, opens a temp database seeded with the
// events in eventsDoc (JSON, see events.Decode; may be empty), and
// starts a fake ntfy server. Everything is cleaned up when the test ends.
func NewEnv(t *testing.T, eventsDoc string) *Env {
	t.Helper()
	FreezeClock(t, FixedNow)
	dir := t.TempDir()

	rec := &Recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		title, _ := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
		rec.mu.Lock()
		rec.sent = append(rec.sent, Notification{
			Topic: strings.TrimPrefix(r.URL.Path, "/"), Title: title, Body: string(body),
			Priority: r.Header.Get("Priority"), Tags: r.Header.Get("Tags"), Auth: r.Header.Get("Authorization"),
		})
		rec.mu.Unlock()
		w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(dir, "gonotifier.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if strings.TrimSpace(eventsDoc) != "" {
		evs, err := events.Decode([]byte(eventsDoc))
		if err != nil {
			t.Fatalf("seed events: %v", err)
		}
		if _, err := st.Import(evs, false); err != nil {
			t.Fatalf("seed events: %v", err)
		}
	}

	return &Env{
		Cfg: &config.Config{
			NtfyURL: srv.URL, NtfyToken: "tk_test", NtfyDefaultTopic: "reminders",
			DBPath: dbPath, ExportFile: filepath.Join(dir, "events-export.json"), TZ: Loc,
			CatchupWindow: 24 * time.Hour, DefaultNotifyTime: events.TimeOnly{Hour: 9}, Port: 8080,
		},
		Store: st,
		Ntfy:  rec,
	}
}

// ID returns the ID of the stored event with the given name.
func (e *Env) ID(t *testing.T, name string) string {
	t.Helper()
	evs, err := e.Store.ListEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Name == name {
			return ev.ID
		}
	}
	t.Fatalf("no event named %q", name)
	return ""
}

// Names returns the names of all stored events, in creation order.
func (e *Env) Names(t *testing.T) []string {
	t.Helper()
	evs, err := e.Store.ListEvents()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Name)
	}
	return out
}

// FreezeClock sets clock.Now to return t until the test ends.
func FreezeClock(tb testing.TB, t time.Time) {
	prev := clock.Now
	clock.Now = func() time.Time { return t }
	tb.Cleanup(func() { clock.Now = prev })
}
