package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
	"github.com/sebibar/gonotifier/internal/notify"
	"github.com/sebibar/gonotifier/internal/store"
)

// Users log in with their ntfy username and password: ntfy checks them, so gonotifier
// stores no passwords. The web UI then uses a session cookie; the API takes the same
// Authorization header ntfy does (an ntfy token, or Basic username:password).

const (
	sessionCookie = "gonotifier_session"
	sessionTTL    = 30 * 24 * time.Hour // renewed while in use
	recheckAfter  = 24 * time.Hour      // how often a session re-checks the user still exists in ntfy

	maxFailures = 5                // wrong passwords in a row before a username is locked
	lockFor     = 15 * time.Minute // how long it stays locked
	apiCacheFor = time.Minute      // how long a checked API Authorization header is trusted
)

type ctxKey struct{}

// userOf returns the logged-in user (set by requireUser / requireAPIUser).
func userOf(r *http.Request) string {
	name, _ := r.Context().Value(ctxKey{}).(string)
	return name
}

func withUser(r *http.Request, name string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, name))
}

// auth holds the in-memory login state: failed attempts per username and checked API headers.
type auth struct {
	mu       sync.Mutex
	failures map[string]*failure
	apiCache map[[32]byte]cached
}

type failure struct {
	count       int
	lockedUntil time.Time
}

type cached struct {
	username string
	until    time.Time
}

func newAuth() *auth {
	return &auth{failures: map[string]*failure{}, apiCache: map[[32]byte]cached{}}
}

func (a *auth) locked(username string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[strings.ToLower(username)]
	return f != nil && clock.Now().Before(f.lockedUntil)
}

func (a *auth) recordFailure(username string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := strings.ToLower(username)
	f := a.failures[key]
	if f == nil {
		f = &failure{}
		a.failures[key] = f
	}
	if f.count++; f.count >= maxFailures {
		f.count, f.lockedUntil = 0, clock.Now().Add(lockFor)
	}
}

func (a *auth) clearFailures(username string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, strings.ToLower(username))
}

// signIn checks an Authorization header with ntfy and makes sure gonotifier has a working
// ntfy token for that user (creating the user on their first login). It returns the username.
func (s *server) signIn(authorization string) (string, error) {
	username, err := s.ntfy.Account(authorization)
	if err != nil {
		return "", err
	}
	u, err := s.store.GetUser(username)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if exists {
		if name, err := s.ntfy.Account("Bearer " + u.NtfyToken); err == nil && name == username {
			return username, nil // stored token still works
		}
	}

	// Need a (new) token: reuse the one they authenticated with, or create one.
	token, isToken := strings.CutPrefix(authorization, "Bearer ")
	if !isToken {
		if token, err = s.ntfy.CreateToken(authorization); err != nil {
			return "", err
		}
	}
	if exists {
		return username, s.store.SetNtfyToken(username, token)
	}
	if _, err := s.store.CreateUser(username, token); err != nil {
		return "", err
	}
	slog.Info("new user", "user", username)
	return username, nil
}

// ---------- web UI: login page and session cookie ----------

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.sessionUser(w, r) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	render(w, r, http.StatusOK, Login("", ""))
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	fail := func(status int, msg string) { render(w, r, status, Login(username, msg)) }

	if username == "" || password == "" || strings.Contains(username, ":") {
		fail(http.StatusUnauthorized, "Wrong username or password.")
		return
	}
	if s.auth.locked(username) {
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	name, err := s.signIn(basic)
	switch {
	case errors.Is(err, notify.ErrUnauthorized):
		s.auth.recordFailure(username)
		fail(http.StatusUnauthorized, "Wrong username or password.")
		return
	case err != nil:
		slog.Error("login", "user", username, "err", err)
		fail(http.StatusBadGateway, "Can't reach ntfy to check your login. Try again later.")
		return
	}
	s.auth.clearFailures(username)

	cookie, err := s.store.CreateSession(name, sessionTTL)
	if err != nil {
		slog.Error("create session", "err", err)
		fail(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}
	setSessionCookie(w, r, cookie, sessionTTL)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(c.Value)
	}
	setSessionCookie(w, r, "", -1) // delete it
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// setSessionCookie sets the session cookie for maxAge (negative deletes it). It is never
// readable by scripts, only sent from this site, and HTTPS-only when the site is.
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	seconds := int(maxAge.Seconds())
	if maxAge < 0 {
		seconds = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: seconds,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
	})
}

// isHTTPS reports whether the browser reached us over HTTPS (directly or through a proxy).
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// sessionUser returns the user of the request's session cookie, or "". Once a day it
// renews the session and checks with ntfy that the user still exists.
func (s *server) sessionUser(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	username, expires, err := s.store.Session(c.Value)
	if err != nil {
		return ""
	}
	if expires.Sub(clock.Now()) > sessionTTL-recheckAfter {
		return username
	}
	if u, err := s.store.GetUser(username); err == nil {
		if _, err := s.ntfy.Account("Bearer " + u.NtfyToken); errors.Is(err, notify.ErrUnauthorized) {
			// Removed from ntfy (or the token was revoked): log them out.
			s.store.DeleteSession(c.Value)
			return ""
		} // if ntfy is just unreachable, keep them logged in
	}
	if err := s.store.RenewSession(c.Value, sessionTTL); err == nil {
		setSessionCookie(w, r, c.Value, sessionTTL)
	}
	return username
}

// requireUser protects web UI routes: without a session, browsers go to the login page.
func (s *server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := s.sessionUser(w, r)
		if name == "" {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login") // htmx does a full page navigation
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, withUser(r, name))
	}
}

// ---------- JSON API ----------

// requireAPIUser protects API routes: a session cookie (so the API works from the browser)
// or an Authorization header that ntfy accepts.
func (s *server) requireAPIUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := s.sessionUser(w, r)
		if name == "" {
			name = s.apiUser(r.Header.Get("Authorization"))
		}
		if name == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="gonotifier"`)
			apiError(w, http.StatusUnauthorized, "log in with an ntfy token (Authorization: Bearer tk_...) or username and password")
			return
		}
		h(w, withUser(r, name))
	}
}

// apiUser checks an Authorization header, trusting a successful check for apiCacheFor
// so scripts don't cause an ntfy request per call.
func (s *server) apiUser(authorization string) string {
	if authorization == "" {
		return ""
	}
	key := sha256.Sum256([]byte(authorization))
	now := clock.Now()
	s.auth.mu.Lock()
	c, ok := s.auth.apiCache[key]
	s.auth.mu.Unlock()
	if ok && now.Before(c.until) {
		return c.username
	}

	name, err := s.signIn(authorization)
	if err != nil {
		if !errors.Is(err, notify.ErrUnauthorized) {
			slog.Error("api auth", "err", err)
		}
		return ""
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	for k, c := range s.auth.apiCache {
		if now.After(c.until) {
			delete(s.auth.apiCache, k)
		}
	}
	s.auth.apiCache[key] = cached{username: name, until: now.Add(apiCacheFor)}
	return name
}
