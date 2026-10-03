package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/store"
)

var jsonHeaders = map[string]string{"Content-Type": "application/json"}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", body, err)
	}
	return v
}

func TestAPIListAndGet(t *testing.T) {
	env, mux, _ := newTestMux(t, sampleEvents)
	rr := do(mux, "GET", "/api/events", "", nil)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status %d", rr.Code)
	}
	list := decode[[]apiEvent](t, rr.Body.Bytes())
	if len(list) != 2 || list[1].Name != "Dentist" || list[1].ID == "" || list[1].Priority != "high" ||
		list[1].Next != "2026-10-05T10:00:00-04:00" {
		t.Errorf("unexpected list: %+v", list)
	}
	if list[0].Repeat != "yearly" || list[0].Next != "2027-03-15T00:00:00-04:00" {
		t.Errorf("birthday: %+v", list[0])
	}

	one := do(mux, "GET", "/api/events/"+env.ID(t, "Dentist"), "", nil)
	if got := decode[apiEvent](t, one.Body.Bytes()); one.Code != 200 || got.Name != "Dentist" {
		t.Errorf("get: %d %+v", one.Code, got)
	}
	if rr := do(mux, "GET", "/api/events/missing", "", nil); rr.Code != 404 {
		t.Errorf("missing: %d", rr.Code)
	}

	_, empty, _ := newTestMux(t, "")
	if body := strings.TrimSpace(do(empty, "GET", "/api/events", "", nil).Body.String()); body != "[]" {
		t.Errorf("empty list = %s", body)
	}
}

func TestAPICreate(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)
	body := `{"id":"ignored","name":"Server maintenance","date":"2026-12-15T02:00","repeat":"monthly","reminders":["1d","12h"],"priority":"high"}`
	rr := do(mux, "POST", "/api/events", body, jsonHeaders)
	if rr.Code != 201 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	created := decode[apiEvent](t, rr.Body.Bytes())
	if created.ID == "" || created.ID == "ignored" || created.Repeat != "monthly" || created.Next == "" {
		t.Errorf("created = %+v", created)
	}
	if _, err := env.Store.GetEvent(created.ID); err != nil {
		t.Errorf("not stored: %v", err)
	}
	if rr := do(mux, "POST", "/api/events", body, jsonHeaders); rr.Code != 400 {
		t.Errorf("duplicate status %d", rr.Code)
	}
	if sched.wakes.Load() != 1 {
		t.Errorf("wakes = %d", sched.wakes.Load())
	}
}

func TestAPIUpdate_RoundTripsGetOutput(t *testing.T) {
	env, mux, _ := newTestMux(t, sampleEvents)
	id := env.ID(t, "Dentist")
	got := do(mux, "GET", "/api/events/"+id, "", nil).Body.String() // includes the read-only "next"
	edited := strings.Replace(got, `"name":"Dentist"`, `"name":"Dentist (moved)"`, 1)
	rr := do(mux, "PUT", "/api/events/"+id, edited, jsonHeaders)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if e, _ := env.Store.GetEvent(id); e.Name != "Dentist (moved)" {
		t.Errorf("not updated: %+v", e)
	}
	if rr := do(mux, "PUT", "/api/events/missing", edited, jsonHeaders); rr.Code != 404 {
		t.Errorf("missing: %d", rr.Code)
	}
}

func TestAPIValidation(t *testing.T) {
	_, mux, _ := newTestMux(t, sampleEvents)
	cases := map[string]string{
		"malformed":     `{"name":`,
		"unknown field": `{"name":"A","date":"2026-12-01","reminders":["1d"],"bogus":1}`,
		"bad date":      `{"name":"A","date":"2026-13-01","reminders":["1d"]}`,
		"no reminders":  `{"name":"A","date":"2026-12-01","reminders":[]}`,
		"bad priority":  `{"name":"A","date":"2026-12-01","reminders":["1d"],"priority":"meh"}`,
		"bad repeat":    `{"name":"A","date":"2026-12-01","reminders":["1d"],"repeat":"hourly"}`,
	}
	for name, body := range cases {
		rr := do(mux, "POST", "/api/events", body, jsonHeaders)
		if got := decode[map[string]any](t, rr.Body.Bytes()); rr.Code != 400 || got["error"] == nil {
			t.Errorf("%s: status %d body %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestAPIDelete(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)
	id := env.ID(t, "Mom's birthday")
	if rr := do(mux, "DELETE", "/api/events/"+id, "", nil); rr.Code != 204 {
		t.Fatalf("status %d", rr.Code)
	}
	if rr := do(mux, "DELETE", "/api/events/"+id, "", nil); rr.Code != 404 {
		t.Errorf("second delete: %d", rr.Code)
	}
	if len(env.Names(t)) != 1 || sched.wakes.Load() != 1 {
		t.Error("delete not applied or scheduler not woken")
	}
}

func TestAPIExportImport(t *testing.T) {
	env, mux, sched := newTestMux(t, sampleEvents)

	export := do(mux, "GET", "/api/export", "", nil)
	if export.Code != 200 || !strings.HasPrefix(export.Header().Get("Content-Type"), "application/json") ||
		!strings.Contains(export.Body.String(), `"events": [`) {
		t.Fatalf("export: %d %s", export.Code, export.Body.String())
	}

	// Edit the export and import it back: one update (by id) and one new event.
	doc := decode[events.Document](t, export.Body.Bytes())
	for i := range doc.Events {
		if doc.Events[i].Name == "Dentist" {
			doc.Events[i].Name = "Dentist moved"
		}
	}
	doc.Events = append(doc.Events, events.Event{Name: "Gym", Date: "2026-10-05T18:00", Repeat: "weekly", Reminders: []string{"1h"}})
	body, _ := json.Marshal(doc)
	rr := do(mux, "POST", "/api/import", string(body), jsonHeaders)
	if rr.Code != 200 {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	if res := decode[store.ImportResult](t, rr.Body.Bytes()); res != (store.ImportResult{Created: 1, Updated: 2}) {
		t.Errorf("import result = %+v", res)
	}
	if names := strings.Join(env.Names(t), ","); names != "Mom's birthday,Dentist moved,Gym" {
		t.Errorf("after import: %s", names)
	}

	// Invalid documents change nothing.
	before := strings.Join(env.Names(t), ",")
	for _, bad := range []string{"events:\n  - name: A", `{"events":[{"name":"Broken"}]}`} {
		if rr := do(mux, "POST", "/api/import", bad, nil); rr.Code != 400 {
			t.Errorf("bad import %q: %d", bad, rr.Code)
		}
	}
	if after := strings.Join(env.Names(t), ","); after != before {
		t.Errorf("failed import changed events: %s", after)
	}

	// Replace mode keeps only what's in the document.
	rr = do(mux, "POST", "/api/import?mode=replace", `[{"name":"Only","date":"2026-12-01","reminders":["1d"]}]`, jsonHeaders)
	if res := decode[store.ImportResult](t, rr.Body.Bytes()); rr.Code != 200 || res.Deleted != 3 || res.Created != 1 {
		t.Errorf("replace: %d %+v", rr.Code, res)
	}
	if names := env.Names(t); len(names) != 1 || names[0] != "Only" {
		t.Errorf("after replace: %v", names)
	}
	if sched.wakes.Load() != 2 {
		t.Errorf("wakes = %d, want 2 (one per successful import)", sched.wakes.Load())
	}
}
