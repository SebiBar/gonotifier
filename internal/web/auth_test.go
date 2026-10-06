package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/testutil"
)

// login posts the login form and returns the session cookie ("" if the login failed).
func login(t *testing.T, h http.Handler, username, password string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	rr := do(h, "POST", "/login", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return rr, sessionCookie + "=" + c.Value
		}
	}
	return rr, ""
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestLoginRequired(t *testing.T) {
	_, h, _ := newApp(t, sampleEvents)
	if rr := do(h, "GET", "/", "", nil); rr.Code != 303 || rr.Header().Get("Location") != "/login" {
		t.Errorf("dashboard without login: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	// htmx requests get a full-page redirect instead of a login page inside the dialog.
	if rr := do(h, "GET", "/events/new", "", map[string]string{"HX-Request": "true"}); rr.Code != 401 || rr.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx without login: %d %q", rr.Code, rr.Header().Get("HX-Redirect"))
	}
	if rr := do(h, "GET", "/login", "", nil); rr.Code != 200 {
		t.Errorf("login page: %d", rr.Code)
	}
}

func TestLogin(t *testing.T) {
	env, h, _ := newApp(t, sampleEvents)

	rr, cookie := login(t, h, testutil.Other, "wrong")
	if rr.Code != 401 || cookie != "" {
		t.Fatalf("wrong password: %d %q", rr.Code, cookie)
	}

	// First login: ntfy checks the password, gonotifier creates the user with their own ntfy token.
	rr, cookie = login(t, h, testutil.Other, testutil.OtherPassword)
	if rr.Code != 303 || rr.Header().Get("Location") != "/" || cookie == "" {
		t.Fatalf("login: %d %q", rr.Code, cookie)
	}
	u, err := env.Store.GetUser(testutil.Other)
	if err != nil || !strings.HasPrefix(u.NtfyToken, "tk_bob_") || u.FeedToken == "" {
		t.Fatalf("user after first login: %+v, %v", u, err)
	}

	// Logged in: their own (empty) dashboard, not testutil.User's events.
	body := do(h, "GET", "/", "", map[string]string{"Cookie": cookie}).Body.String()
	if !strings.Contains(body, ">bob<") || strings.Contains(body, "Dentist") {
		t.Error("dashboard should show bob's name and none of alice's events")
	}
	if rr := do(h, "GET", "/login", "", map[string]string{"Cookie": cookie}); rr.Code != 303 {
		t.Errorf("login page while logged in: %d", rr.Code)
	}

	// Logging in again keeps the same ntfy token.
	login(t, h, testutil.Other, testutil.OtherPassword)
	if again, _ := env.Store.GetUser(testutil.Other); again.NtfyToken != u.NtfyToken {
		t.Errorf("token replaced on second login: %q → %q", u.NtfyToken, again.NtfyToken)
	}

	// Logout ends the session.
	do(h, "POST", "/logout", "", map[string]string{"Cookie": cookie})
	if rr := do(h, "GET", "/", "", map[string]string{"Cookie": cookie}); rr.Code != 303 {
		t.Errorf("still logged in after logout: %d", rr.Code)
	}
}

func TestLogin_LockedAfterFailures(t *testing.T) {
	_, h, _ := newApp(t, "")
	for range maxFailures {
		login(t, h, testutil.Other, "wrong")
	}
	if rr, cookie := login(t, h, testutil.Other, testutil.OtherPassword); rr.Code != 429 || cookie != "" {
		t.Fatalf("locked account accepted the right password: %d", rr.Code)
	}
	testutil.FreezeClock(t, testutil.FixedNow.Add(lockFor))
	if _, cookie := login(t, h, testutil.Other, testutil.OtherPassword); cookie == "" {
		t.Error("still locked after lockFor")
	}
}

func TestSession_LoggedOutWhenRemovedFromNtfy(t *testing.T) {
	env, h, _ := newApp(t, "")
	_, cookie := login(t, h, testutil.User, testutil.UserPassword)
	headers := map[string]string{"Cookie": cookie}

	// Within a day the session isn't re-checked.
	env.Ntfy.RevokeTokens(testutil.User)
	testutil.FreezeClock(t, testutil.FixedNow.Add(time.Hour))
	if rr := do(h, "GET", "/", "", headers); rr.Code != 200 {
		t.Fatalf("session not valid: %d", rr.Code)
	}
	// After a day it is, and the user's token no longer works in ntfy.
	testutil.FreezeClock(t, testutil.FixedNow.Add(recheckAfter+time.Hour))
	if rr := do(h, "GET", "/", "", headers); rr.Code != 303 {
		t.Errorf("removed user still logged in: %d", rr.Code)
	}
}

func TestSession_Renewed(t *testing.T) {
	_, h, _ := newApp(t, "")
	_, cookie := login(t, h, testutil.User, testutil.UserPassword)
	headers := map[string]string{"Cookie": cookie}
	// Used every few weeks, a session never expires.
	for i := 1; i <= 3; i++ {
		testutil.FreezeClock(t, testutil.FixedNow.Add(time.Duration(i)*20*24*time.Hour))
		if rr := do(h, "GET", "/", "", headers); rr.Code != 200 {
			t.Fatalf("after %d×20 days: %d", i, rr.Code)
		}
	}
	testutil.FreezeClock(t, testutil.FixedNow.Add(60*24*time.Hour+sessionTTL))
	if rr := do(h, "GET", "/", "", headers); rr.Code != 303 {
		t.Errorf("unused session didn't expire: %d", rr.Code)
	}
}

func TestAPIAuth(t *testing.T) {
	env, h, _ := newApp(t, sampleEvents)
	if rr := do(h, "GET", "/api/events", "", nil); rr.Code != 401 || rr.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no auth: %d", rr.Code)
	}
	if rr := do(h, "GET", "/api/events", "", map[string]string{"Authorization": "Bearer tk_nope"}); rr.Code != 401 {
		t.Errorf("bad token: %d", rr.Code)
	}

	// An ntfy token works, and only shows its own user's events.
	alice := map[string]string{"Authorization": "Bearer " + testutil.UserToken, "Content-Type": "application/json"}
	if list := decode[[]apiEvent](t, do(h, "GET", "/api/events", "", alice).Body.Bytes()); len(list) != 2 {
		t.Errorf("alice sees %d events, want 2", len(list))
	}

	// Username and password work too, even for someone who never used the web UI.
	bob := map[string]string{"Authorization": basic(testutil.Other, testutil.OtherPassword), "Content-Type": "application/json"}
	if body := strings.TrimSpace(do(h, "GET", "/api/events", "", bob).Body.String()); body != "[]" {
		t.Errorf("bob sees %s", body)
	}
	rr := do(h, "POST", "/api/events", `{"name":"Dentist","date":"2026-10-05T10:00","reminders":["1d"]}`, bob)
	if rr.Code != 201 { // same name and date as alice's: fine, they're separate users
		t.Fatalf("bob create: %d %s", rr.Code, rr.Body.String())
	}
	bobsID := decode[apiEvent](t, rr.Body.Bytes()).ID
	if u, err := env.Store.GetUser(testutil.Other); err != nil || u.NtfyToken == "" {
		t.Errorf("API-only user has no ntfy token: %+v %v", u, err)
	}

	// Neither can see or change the other's events.
	alicesID := env.ID(t, "Dentist")
	for _, req := range []struct{ method, path string }{
		{"GET", "/api/events/" + bobsID}, {"PUT", "/api/events/" + bobsID}, {"DELETE", "/api/events/" + bobsID},
	} {
		if rr := do(h, req.method, req.path, `{"name":"Hacked","date":"2026-12-01","reminders":["1d"]}`, alice); rr.Code != 404 {
			t.Errorf("alice %s bob's event: %d", req.method, rr.Code)
		}
	}
	// Importing with someone else's id creates a new event instead of overwriting theirs.
	if rr := do(h, "POST", "/api/import", `[{"id":"`+alicesID+`","name":"Mine now","date":"2026-12-01","reminders":["1d"]}]`, bob); rr.Code != 200 {
		t.Fatalf("bob import: %d %s", rr.Code, rr.Body.String())
	}
	if e := get(t, env, "Dentist"); e.ID != alicesID {
		t.Errorf("alice's event changed: %+v", e)
	}
	if rr := do(h, "POST", "/api/import?mode=replace", `[]`, bob); rr.Code != 200 {
		t.Fatalf("bob replace: %d", rr.Code)
	}
	if n := len(env.Names(t)); n != 2 {
		t.Errorf("bob's replace import deleted alice's events: %d left", n)
	}
}

func TestUIUsersAreSeparate(t *testing.T) {
	env, h, _ := newApp(t, sampleEvents)
	_, cookie := login(t, h, testutil.Other, testutil.OtherPassword)
	bob := map[string]string{"Cookie": cookie, "HX-Request": "true", "Content-Type": "application/x-www-form-urlencoded"}
	id := env.ID(t, "Dentist") // alice's
	form := url.Values{"name": {"Hacked"}, "date": {"2026-12-01"}, "reminders": {"1d"}}.Encode()

	if rr := do(h, "GET", "/events/"+id+"/edit", "", bob); rr.Code != 404 {
		t.Errorf("bob opened alice's event: %d", rr.Code)
	}
	do(h, "PUT", "/events/"+id, form, bob)
	do(h, "DELETE", "/events/"+id, "", bob)
	if e := get(t, env, "Dentist"); e.Name != "Dentist" {
		t.Errorf("bob changed alice's event: %+v", e)
	}
}

func TestCrossSiteRequestsRejected(t *testing.T) {
	env, mux, _ := newTestMux(t, sampleEvents)
	form := url.Values{"name": {"From evil.example"}, "date": {"2026-12-01"}, "reminders": {"1d"}}.Encode()
	rr := do(mux, "POST", "/events", form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site",
	})
	if rr.Code != 403 {
		t.Errorf("cross-site form post: %d", rr.Code)
	}
	if len(env.Names(t)) != 2 {
		t.Error("cross-site post created an event")
	}
	if rr := do(mux, "GET", "/", "", nil); rr.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("missing security headers")
	}
}
