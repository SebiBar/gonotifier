// Package testutil provides shared fixtures for package tests: a temp SQLite store
// seeded with events, a fake ntfy server with two accounts, and a frozen clock.
package testutil

import (
	"encoding/json"
	"fmt"
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

// User is the account seeded events belong to. The fake ntfy server also knows Other.
const (
	User          = "alice"
	UserPassword  = "alice-pw"
	UserToken     = "tk_alice"
	Other         = "bob"
	OtherPassword = "bob-pw"
	OtherToken    = "tk_bob"
)

// Recorder is the fake ntfy server's state: its accounts and the notifications it received.
type Recorder struct {
	mu        sync.Mutex
	sent      []Notification
	passwords map[string]string // username → password
	tokens    map[string]string // token → username
}

// account returns the user an Authorization header belongs to: "" for none, "*" for anonymous.
func (r *Recorder) account(authorization string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tok, ok := strings.CutPrefix(authorization, "Bearer "); ok {
		return r.tokens[tok]
	}
	req := http.Request{Header: http.Header{"Authorization": {authorization}}}
	if user, pass, ok := req.BasicAuth(); ok {
		if pw, known := r.passwords[user]; known && pw == pass {
			return user
		}
		return ""
	}
	return "*"
}

// RevokeTokens deletes all of a user's tokens, like deleting them in ntfy.
func (r *Recorder) RevokeTokens(username string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for tok, user := range r.tokens {
		if user == username {
			delete(r.tokens, tok)
		}
	}
}

func (r *Recorder) serveHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/v1/account" && req.Method == http.MethodGet:
		user := r.account(req.Header.Get("Authorization"))
		if user == "" {
			http.Error(w, `{"code":40101}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"username": user})
	case req.URL.Path == "/v1/account/token" && req.Method == http.MethodPost:
		user := r.account(req.Header.Get("Authorization"))
		if user == "" || user == "*" {
			http.Error(w, `{"code":40101}`, http.StatusUnauthorized)
			return
		}
		r.mu.Lock()
		tok := fmt.Sprintf("tk_%s_%d", user, len(r.tokens))
		r.tokens[tok] = user
		r.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"token": tok})
	default: // publish
		body, _ := io.ReadAll(req.Body)
		title, _ := new(mime.WordDecoder).DecodeHeader(req.Header.Get("Title"))
		r.mu.Lock()
		r.sent = append(r.sent, Notification{
			Topic: strings.TrimPrefix(req.URL.Path, "/"), Title: title, Body: string(body),
			Priority: req.Header.Get("Priority"), Tags: req.Header.Get("Tags"), Auth: req.Header.Get("Authorization"),
		})
		r.mu.Unlock()
		w.Write([]byte(`{"id":"x"}`))
	}
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

// NewEnv freezes clock.Now at FixedNow, starts a fake ntfy server, and opens a temp
// database with User already logged in once and owning the events in eventsDoc
// (JSON, see events.Decode; may be empty). Everything is cleaned up when the test ends.
func NewEnv(t *testing.T, eventsDoc string) *Env {
	t.Helper()
	FreezeClock(t, FixedNow)
	dir := t.TempDir()

	rec := &Recorder{
		passwords: map[string]string{User: UserPassword, Other: OtherPassword},
		tokens:    map[string]string{UserToken: User, OtherToken: Other},
	}
	srv := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(dir, "gonotifier.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.CreateUser(User, UserToken); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(eventsDoc) != "" {
		evs, err := events.Decode([]byte(eventsDoc))
		if err != nil {
			t.Fatalf("seed events: %v", err)
		}
		if _, err := st.Import(User, evs, false); err != nil {
			t.Fatalf("seed events: %v", err)
		}
	}

	return &Env{
		Cfg: &config.Config{
			NtfyURL: srv.URL,
			DBPath:  dbPath, ExportDir: filepath.Join(dir, "exports"), TZ: Loc,
			CatchupWindow: 24 * time.Hour, DefaultDayStart: events.TimeOnly{Hour: 9}, Port: 8080,
		},
		Store: st,
		Ntfy:  rec,
	}
}

// ID returns the ID of User's event with the given name.
func (e *Env) ID(t *testing.T, name string) string {
	t.Helper()
	evs, err := e.Store.ListEvents(User)
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

// Names returns the names of User's events, in creation order.
func (e *Env) Names(t *testing.T) []string {
	t.Helper()
	evs, err := e.Store.ListEvents(User)
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

// DownURL returns the address of a server that has stopped, so requests to it fail at once
// (a made-up address like 127.0.0.1:1 can hang until the client's timeout instead).
func DownURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}
