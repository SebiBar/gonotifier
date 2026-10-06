package web

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/testutil"
)

// Clock is frozen at testutil.FixedNow: Fri 2 Oct 2026, 10:00 (America/New_York).
const sampleEvents = `[
	{"name": "Mom's birthday", "date": "2026-03-15", "repeat": "yearly", "reminders": ["1d", "5d"]},
	{"name": "Dentist", "date": "2026-10-05T10:00", "reminders": ["12h", "1d"], "priority": "high"}
]`

type fakeSched struct {
	wakes atomic.Int32
	next  time.Time
}

func (f *fakeSched) Wake()                        { f.wakes.Add(1) }
func (f *fakeSched) LastCheck() (time.Time, bool) { return testutil.FixedNow, true }
func (f *fakeSched) Next() (time.Time, bool)      { return f.next, !f.next.IsZero() }

// newTestMux returns the app with every request logged in as testutil.User
// (unless the request already carries a cookie).
func newTestMux(t *testing.T, doc string) (*testutil.Env, http.Handler, *fakeSched) {
	t.Helper()
	env, h, sched := newApp(t, doc)
	cookie, err := env.Store.CreateSession(testutil.User, sessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	return env, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		h.ServeHTTP(w, r)
	}), sched
}

// newApp returns the app as an anonymous visitor sees it.
func newApp(t *testing.T, doc string) (*testutil.Env, http.Handler, *fakeSched) {
	t.Helper()
	env := testutil.NewEnv(t, doc)
	sched := &fakeSched{next: time.Date(2026, 10, 4, 9, 0, 0, 0, testutil.Loc)}
	return env, Handler(env.Cfg, env.Store, sched), sched
}

func do(mux http.Handler, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

var formHeaders = map[string]string{"Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true"}

// alpineState decodes the form's x-data attribute, which must be valid JSON.
func alpineState(t *testing.T, body string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`x-data="(\{&#34;[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no JSON x-data in form: %s", body)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &state); err != nil {
		t.Fatalf("x-data is not valid JSON: %v: %s", err, m[1])
	}
	return state
}

func get(t *testing.T, env *testutil.Env, name string) events.Event {
	t.Helper()
	e, err := env.Store.GetEvent(testutil.User, env.ID(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDashboard_RendersEvents(t *testing.T) {
	_, mux, _ := newTestMux(t, sampleEvents)
	rr := do(mux, "GET", "/", "", nil)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Mom&#39;s birthday", "Dentist", `id="events-count">2<`,
		`<span class="mon">OCT</span><span class="day">5</span>`, "in 3 days", "Yearly", "Mar 15"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if rr := do(mux, "GET", "/nope", "", nil); rr.Code != 404 {
		t.Errorf("unknown path status %d", rr.Code)
	}
}

func TestEventForms(t *testing.T) {
	env, mux, _ := newTestMux(t, sampleEvents)
	rr := do(mux, "GET", "/events/new", "", nil)
	if rr.Code != 200 {
		t.Errorf("new form: %d", rr.Code)
	}
	if rr.Header().Get("HX-Trigger-After-Swap") != "open-dialog" {
		t.Error("form response should open the dialog after swap")
	}

	id := env.ID(t, "Dentist")
	body := do(mux, "GET", "/events/"+id+"/edit", "", nil).Body.String()
	for _, want := range []string{`hx-put="/events/` + id + `"`, `name="date" value="2026-10-05"`, `name="time" value="10:00"`,
		`value="high" checked`, `value="1d" checked`,
		`name="custom_reminders" value="12h"`} { // not one of the choices: shown under "Other"
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}
	if state := alpineState(t, body); state["repeat"] != "" || state["more"] != true {
		t.Errorf("edit form x-data = %v", state)
	}
	bday := do(mux, "GET", "/events/"+env.ID(t, "Mom's birthday")+"/edit", "", nil).Body.String()
	if alpineState(t, bday)["repeat"] != "yearly" || !strings.Contains(bday, `name="date" value="2026-03-15"`) {
		t.Errorf("yearly event not pre-populated: %s", bday)
	}
	if rr := do(mux, "GET", "/events/doesnotexist/edit", "", nil); rr.Code != 404 {
		t.Errorf("missing event edit status %d", rr.Code)
	}
}

func TestAddEvent_Success(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)
	form := url.Values{
		"name": {"Renew car insurance"}, "date": {"2026-12-01"}, "time": {""}, "repeat": {""},
		"reminders": {"1d,7d"}, "custom_reminders": {"0d"}, "auto_remove": {"on"}, "priority": {"default"},
	}
	rr := do(mux, "POST", "/events", form.Encode(), formHeaders)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if trig := rr.Header().Get("HX-Trigger"); !strings.Contains(trig, "close-dialog") {
		t.Errorf("HX-Trigger = %q", trig)
	}
	e := get(t, env, "Renew car insurance")
	if e.Date != "2026-12-01" || strings.Join(e.Reminders, ",") != "0m,1d,7d" || e.AutoRemove != nil || e.Priority != "" || e.Repeat != "" {
		t.Errorf("stored event = %+v", e)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `class="event-row flash" id="event-`+e.ID) || !strings.Contains(body, `hx-swap-oob="innerHTML"`) {
		t.Errorf("response should be the list with the new row highlighted: %s", body)
	}
	if sched.wakes.Load() != 1 {
		t.Errorf("scheduler woken %d times, want 1", sched.wakes.Load())
	}
}

func TestAddEvent_Repeating(t *testing.T) {
	env, mux, _ := newTestMux(t, "")
	form := url.Values{
		"name": {"Water filter"}, "date": {"2026-10-15"}, "time": {"18:30"}, "repeat": {"monthly"},
		"every": {"3"}, "until": {"2027-12-31"}, "reminders": {"1d"}, "auto_remove": {"on"},
	}
	if rr := do(mux, "POST", "/events", form.Encode(), formHeaders); rr.Code != 200 || rr.Header().Get("HX-Retarget") != "" {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	e := get(t, env, "Water filter")
	if e.Date != "2026-10-15T18:30" || e.Repeat != events.Monthly || e.Every != 3 || e.Until != "2027-12-31" {
		t.Errorf("stored = %+v", e)
	}
	if body := do(mux, "GET", "/", "", nil).Body.String(); !strings.Contains(body, "Every 3 months until Dec 31, 2027") {
		t.Error("dashboard doesn't describe the repeat")
	}
}

func TestEditEvent_KeepsID(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)
	id := env.ID(t, "Dentist")
	form := url.Values{
		"name": {"Dentist (Dr. Smith)"}, "date": {"2026-10-06"}, "time": {"11:30"}, "repeat": {""},
		"reminders": {"1d"}, "priority": {"urgent"}, "topic": {"health"},
	}
	rr := do(mux, "PUT", "/events/"+id, form.Encode(), formHeaders)
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("HX-Trigger"), "close-dialog") {
		t.Fatalf("status %d trigger %q", rr.Code, rr.Header().Get("HX-Trigger"))
	}
	e, err := env.Store.GetEvent(testutil.User, id)
	if err != nil {
		t.Fatalf("event lost its ID: %v", err)
	}
	if e.Name != "Dentist (Dr. Smith)" || e.Date != "2026-10-06T11:30" || e.Priority != "urgent" || e.Topic != "health" ||
		e.AutoRemove == nil || *e.AutoRemove {
		t.Errorf("edited event = %+v", e)
	}
	if sched.wakes.Load() != 1 {
		t.Error("scheduler not woken")
	}
}

func TestAddEvent_Validation(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)
	base := func(kv ...string) url.Values {
		v := url.Values{"name": {"X"}, "date": {"2026-12-01"}, "reminders": {"1d"}}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	// Which inputs are invalid is tested in package events; here: the form stays open with
	// the errors, and whatever was typed comes back as data, never as code.
	cases := map[string]url.Values{
		"invalid":         base("date", "2026-13-45"),
		"script injected": base("repeat", "x' + alert(1) + '"),
	}
	for name, form := range cases {
		rr := do(mux, "POST", "/events", form.Encode(), formHeaders)
		body := rr.Body.String()
		if rr.Code != 200 || rr.Header().Get("HX-Retarget") != "#dialog-content" {
			t.Errorf("%s: expected form with errors, got %d: %s", name, rr.Code, body)
		}
		// Whatever was typed must come back as data inside x-data's JSON, never as code.
		if state := alpineState(t, body); name == "script injected" && state["repeat"] != "x' + alert(1) + '" {
			t.Errorf("%s: x-data = %v", name, state)
		}
	}
	if n := len(env.Names(t)); n != 2 {
		t.Errorf("invalid submissions changed the events: %d", n)
	}
	if sched.wakes.Load() != 0 {
		t.Error("scheduler woken for rejected input")
	}
}

func TestDeleteEvent(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)
	id := env.ID(t, "Dentist")
	rr := do(mux, "DELETE", "/events/"+id, "", map[string]string{"HX-Request": "true"})
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="events-count" hx-swap-oob="true">1<`) || strings.Contains(body, "event-row") {
		t.Errorf("unexpected delete response: %s", body)
	}
	if sched.wakes.Load() != 1 {
		t.Error("scheduler not woken")
	}
	if names := env.Names(t); len(names) != 1 || names[0] != "Mom's birthday" {
		t.Errorf("events after delete: %v", names)
	}
}

func TestFeed(t *testing.T) {
	env, mux, _ := newTestMux(t, sampleEvents)
	_, anon, _ := newApp(t, "")
	u, err := env.Store.GetUser(testutil.User)
	if err != nil {
		t.Fatal(err)
	}
	feed := "/feed/" + u.FeedToken + ".ics"

	// Calendar apps fetch the feed without logging in: the URL is the secret.
	rr := do(mux, "GET", feed, "", map[string]string{"Cookie": "none=1"})
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/calendar; charset=utf-8" {
		t.Fatalf("feed: status %d content-type %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if body := rr.Body.String(); !strings.HasPrefix(body, "BEGIN:VCALENDAR") || strings.Count(body, "BEGIN:VEVENT") != 2 {
		t.Errorf("bad feed:\n%s", body)
	}
	for _, path := range []string{"/feed.ics", "/feed/wrong-token-abcdef1234567.ics", "/feed/.ics",
		"/feed/" + u.FeedToken, "/feed/" + u.FeedToken + ".ics.bak"} {
		if rr := do(anon, "GET", path, "", nil); rr.Code != 404 {
			t.Errorf("%s: status %d, want 404", path, rr.Code)
		}
	}
	// The dashboard shows the user's secret URL so it can be copied (not opened: on a phone
	// that imports a copy that never updates).
	body := do(mux, "GET", "/", "", nil).Body.String()
	if !strings.Contains(body, `value="`+feed+`"`) {
		t.Error("dashboard doesn't show the feed URL to copy")
	}

	// Replacing the link: the old one stops working, the new one works.
	rr = do(mux, "POST", "/feed/reset", "", map[string]string{"HX-Request": "true"})
	u2, _ := env.Store.GetUser(testutil.User)
	newFeed := "/feed/" + u2.FeedToken + ".ics"
	if rr.Code != 200 || newFeed == feed || !strings.Contains(rr.Body.String(), `value="`+newFeed+`"`) {
		t.Fatalf("reset: %d %s", rr.Code, rr.Body.String())
	}
	if do(mux, "GET", feed, "", nil).Code != 404 || do(mux, "GET", newFeed, "", nil).Code != 200 {
		t.Error("old link still works or new link doesn't")
	}
}

func TestFeedURL_ShownOnPublicHost(t *testing.T) {
	env, mux, _ := newTestMux(t, sampleEvents)
	env.Cfg.FeedURL = "https://cal.example.com"
	u, _ := env.Store.GetUser(testutil.User)
	if body := do(mux, "GET", "/", "", nil).Body.String(); !strings.Contains(body, `value="https://cal.example.com/feed/`+u.FeedToken+`.ics"`) {
		t.Error("dashboard doesn't link to the feed on FEED_URL")
	}
	rr := do(mux, "POST", "/feed/reset", "", map[string]string{"HX-Request": "true"})
	if !strings.Contains(rr.Body.String(), `value="https://cal.example.com/feed/`) {
		t.Errorf("reset doesn't use FEED_URL: %s", rr.Body.String())
	}
}

func TestHealth(t *testing.T) {
	_, mux, _ := newTestMux(t, sampleEvents)
	for _, path := range []string{"/health", "/api/health"} {
		rr := do(mux, "GET", path, "", nil)
		var got map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: invalid JSON: %v", path, err)
		}
		if rr.Code != 200 || got["status"] != "ok" || got["events"] != float64(2) ||
			got["last_check"] == nil || got["next_reminder"] != "2026-10-04T09:00:00-04:00" {
			t.Errorf("%s: %d %v", path, rr.Code, got)
		}
	}
}

func TestStaticAssets(t *testing.T) {
	_, mux, _ := newTestMux(t, sampleEvents)
	page := do(mux, "GET", "/", "", nil).Body.String()
	if !strings.Contains(page, "/static/app.css?v="+assetVersion) {
		t.Error("dashboard does not reference versioned stylesheet")
	}
	rr := do(mux, "GET", "/static/app.css?v="+assetVersion, "", nil)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/css") ||
		!strings.Contains(rr.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("app.css: %d %q %q", rr.Code, rr.Header().Get("Content-Type"), rr.Header().Get("Cache-Control"))
	}
	for _, f := range []string{"favicon.svg", "vendor/htmx.min.js", "vendor/alpine.min.js", "vendor/pico.min.css", "vendor/inter-latin.woff2", "vendor/inter-latin-ext.woff2"} {
		if rr := do(mux, "GET", "/static/"+f, "", nil); rr.Code != 200 || rr.Body.Len() < 100 {
			t.Errorf("%s: status %d, %d bytes", f, rr.Code, rr.Body.Len())
		}
	}
	if cc := do(mux, "GET", "/static/vendor/inter-latin.woff2", "", nil).Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("unversioned URL must not be cached as immutable: %q", cc)
	}
}

func TestForm_Reminders(t *testing.T) {
	env, mux, _ := newTestMux(t, "")
	body := do(mux, "GET", "/events/new", "", nil).Body.String()
	// Like calendar apps: today's date and "On time" are filled in, so a simple event needs
	// only a name (and a time).
	for _, want := range []string{`name="date" value="2026-10-02"`, `value="0m" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("new form missing %q", want)
		}
	}

	// "On time" on a daily event: for simple reminders like taking medicine.
	form := url.Values{"name": {"Take medicine"}, "date": {"2026-10-02"}, "time": {"20:00"}, "repeat": {"daily"}, "reminders": {"0m"}}
	if rr := do(mux, "POST", "/events", form.Encode(), formHeaders); rr.Header().Get("HX-Retarget") != "" {
		t.Fatalf("on-time reminder rejected: %s", rr.Body.String())
	}
	if e := get(t, env, "Take medicine"); strings.Join(e.Reminders, ",") != "0m" {
		t.Errorf("reminders = %v", e.Reminders)
	}

	// All-day events take the same reminders: they count back from the start of the day.
	form = url.Values{"name": {"Insurance"}, "date": {"2026-12-01"}, "reminders": {"1h", "1d"}}
	if rr := do(mux, "POST", "/events", form.Encode(), formHeaders); rr.Header().Get("HX-Retarget") != "" {
		t.Errorf("hours on an all-day event rejected: %s", rr.Body.String())
	}
}

func TestSave_RejectsWhatIsAlreadyPast(t *testing.T) {
	// Clock: Fri Oct 2, 10:00.
	env, mux, sched := newTestMux(t, "")

	// Form: an event tomorrow with a "3 days before" reminder.
	form := url.Values{"name": {"Dentist"}, "date": {"2026-10-03"}, "time": {"09:00"}, "reminders": {"1h"}, "custom_reminders": {"3d"}}
	rr := do(mux, "POST", "/events", form.Encode(), formHeaders)
	if rr.Header().Get("HX-Retarget") != "#dialog-content" {
		t.Errorf("past reminder accepted: %s", rr.Body.String())
	}
	// Form: an event that already happened.
	form = url.Values{"name": {"Breakfast"}, "date": {"2026-10-02"}, "time": {"08:00"}, "reminders": {"0m"}}
	if rr := do(mux, "POST", "/events", form.Encode(), formHeaders); rr.Header().Get("HX-Retarget") != "#dialog-content" {
		t.Errorf("past event accepted: %s", rr.Body.String())
	}
	// API: same rules.
	rr = do(mux, "POST", "/api/events", `{"name":"Dentist","date":"2026-10-03T09:00","reminders":["3d"]}`, jsonHeaders)
	if rr.Code != 400 {
		t.Errorf("API accepted a past reminder: %d %s", rr.Code, rr.Body.String())
	}
	if len(env.Names(t)) != 0 || sched.wakes.Load() != 0 {
		t.Error("rejected input was saved")
	}

	// Imports skip the check, so a backup with old events can be restored...
	rr = do(mux, "POST", "/api/import", `[{"name":"Last year's trip","date":"2025-07-01","reminders":["1d"],"auto_remove":false}]`, jsonHeaders)
	if rr.Code != 200 {
		t.Fatalf("import of a past event rejected: %d %s", rr.Code, rr.Body.String())
	}
	// ...and a kept event that has passed can still be edited, as long as its date stays.
	id := env.ID(t, "Last year's trip")
	form = url.Values{"name": {"Trip to Rome"}, "date": {"2025-07-01"}, "reminders": {"1d"}}
	if rr := do(mux, "PUT", "/events/"+id, form.Encode(), formHeaders); rr.Header().Get("HX-Retarget") != "" {
		t.Errorf("renaming a kept past event rejected: %s", rr.Body.String())
	}
	if rr := do(mux, "PUT", "/api/events/"+id, `{"name":"Trip to Rome","date":"2025-07-02","reminders":["1d"]}`, jsonHeaders); rr.Code != 400 {
		t.Errorf("moving an event to another past date accepted: %d", rr.Code)
	}
	if rr := do(mux, "PUT", "/api/events/missing", `{"name":"X","date":"2026-12-01","reminders":["1d"]}`, jsonHeaders); rr.Code != 404 {
		t.Errorf("update of a missing event: %d", rr.Code)
	}
}
