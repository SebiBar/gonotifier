package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebibar/gonotifier/internal/events"
	"github.com/sebibar/gonotifier/internal/store"
	"github.com/sebibar/gonotifier/internal/testutil"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	testutil.FreezeClock(t, testutil.FixedNow)
	path := filepath.Join(t.TempDir(), "gn.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// u owns the events in these tests (owners don't need a user row).
const u = "alice"

var dentist = events.Event{Name: "Dentist", Date: "2026-11-15T10:00", Reminders: []string{"1d", "12h"}, Priority: "high"}

func TestEventCRUD(t *testing.T) {
	st, _ := open(t)
	v0 := st.Version()

	created, err := st.CreateEvent(u, dentist)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.ID) != 12 {
		t.Fatalf("ID = %q", created.ID)
	}
	if got, err := st.GetEvent(u, created.ID); err != nil || got.Name != "Dentist" || got.Priority != "high" ||
		len(got.Reminders) != 2 || got.Reminders[0] != "12h" {
		t.Errorf("GetEvent = %+v, %v", got, err)
	}

	// Renaming keeps the ID — that's what keeps sent-reminder history attached.
	renamed := dentist
	renamed.Name = "Dentist (Dr. Smith)"
	renamed.AutoRemove = new(bool)
	updated, err := st.UpdateEvent(u, created.ID, renamed)
	if err != nil || updated.ID != created.ID {
		t.Fatalf("UpdateEvent = %+v, %v", updated, err)
	}
	if got, _ := st.GetEvent(u, created.ID); got.Name != "Dentist (Dr. Smith)" || got.AutoRemove == nil || *got.AutoRemove {
		t.Errorf("after update: %+v", got)
	}

	if list, _ := st.ListEvents(u); len(list) != 1 {
		t.Errorf("ListEvents len = %d", len(list))
	}
	if err := st.DeleteEvent(u, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetEvent(u, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEvent after delete: %v", err)
	}
	if err := st.DeleteEvent(u, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, err := st.UpdateEvent(u, "missing", dentist); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	if st.Version() == v0 {
		t.Error("Version did not change on writes")
	}
}

func TestCreateEvent_ValidatesAndRejectsDuplicates(t *testing.T) {
	st, _ := open(t)
	_, err := st.CreateEvent(u, events.Event{Name: "", Date: "nope"})
	var verrs events.ValidationError
	if !errors.As(err, &verrs) || len(verrs) < 2 {
		t.Errorf("expected validation errors, got %v", err)
	}
	if _, err := st.CreateEvent(u, dentist); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateEvent(u, dentist); !errors.As(err, &verrs) {
		t.Errorf("duplicate accepted: %v", err)
	}
	if list, _ := st.ListEvents(u); len(list) != 1 {
		t.Errorf("events = %d, want 1", len(list))
	}
}

func TestImport(t *testing.T) {
	st, _ := open(t)
	existing, _ := st.CreateEvent(u, dentist)
	keep, _ := st.CreateEvent(u, events.Event{Name: "Keep", Date: "2026-12-01", Reminders: []string{"1d"}})

	// Update by id + create new.
	changed := existing
	changed.Name = "Dentist moved"
	res, err := st.Import(u, []events.Event{changed, {Name: "Mom's birthday", Date: "2026-03-15", Repeat: "yearly", Reminders: []string{"1d"}}}, false)
	if err != nil || res != (store.ImportResult{Created: 1, Updated: 1}) {
		t.Fatalf("Import = %+v, %v", res, err)
	}
	if got, _ := st.GetEvent(u, existing.ID); got.Name != "Dentist moved" {
		t.Errorf("not updated: %+v", got)
	}
	list, _ := st.ListEvents(u)
	if len(list) != 3 || list[2].Repeat != events.Yearly || list[2].Date != "2026-03-15" {
		t.Errorf("after import: %+v", list)
	}

	// All-or-nothing: one invalid event rejects the batch.
	_, err = st.Import(u, []events.Event{{Name: "Fine", Date: "2026-12-02", Reminders: []string{"1d"}}, {Name: "Broken"}}, false)
	var verrs events.ValidationError
	if !errors.As(err, &verrs) {
		t.Errorf("expected validation error, got %v", err)
	}
	if list, _ := st.ListEvents(u); len(list) != 3 {
		t.Errorf("failed import wrote events: %d", len(list))
	}

	// Replace deletes events missing from the document.
	res, err = st.Import(u, []events.Event{keep}, true)
	if err != nil || res != (store.ImportResult{Updated: 1, Deleted: 2}) {
		t.Errorf("replace = %+v, %v", res, err)
	}
	if list, _ := st.ListEvents(u); len(list) != 1 || list[0].ID != keep.ID {
		t.Errorf("after replace: %+v", list)
	}

	// Bad and duplicate ids are rejected.
	if _, err := st.Import(u, []events.Event{{ID: "Bad ID!", Name: "X", Date: "2026-12-03", Reminders: []string{"1d"}}}, false); err == nil {
		t.Error("invalid id accepted")
	}
	dup := events.Event{ID: "abc", Name: "X", Date: "2026-12-03", Reminders: []string{"1d"}}
	if _, err := st.Import(u, []events.Event{dup, dup}, false); err == nil {
		t.Error("duplicate id accepted")
	}
}

func TestSentLog(t *testing.T) {
	st, _ := open(t)
	key := store.ReminderKey("ev1", "1d", "2026-10-03")
	if key == store.ReminderKey("ev1", "1d", "2027-10-03") || len(key) != 16 {
		t.Errorf("keys must differ per occurrence: %q", key)
	}
	if sent, _ := st.IsSent(key); sent {
		t.Error("fresh key reported as sent")
	}
	rec := store.Record{ID: key, Owner: u, EventID: "ev1", EventName: "Dentist", Offset: "1d", TargetDate: "2026-10-03",
		FireTime: testutil.FixedNow, Message: "Dentist is tomorrow"}
	if err := st.RecordSent(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSent(rec); err != nil { // idempotent
		t.Fatal(err)
	}
	if sent, _ := st.IsSent(key); !sent {
		t.Error("recorded key not reported as sent")
	}
	hist, err := st.RecentHistory(u, 10)
	if err != nil || len(hist) != 1 || hist[0].Message != "Dentist is tomorrow" || !hist[0].SentAt.Equal(testutil.FixedNow) {
		t.Errorf("history = %+v, %v", hist, err)
	}

	testutil.FreezeClock(t, testutil.FixedNow.Add(400*24*time.Hour))
	if err := st.Prune(365 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if hist, _ := st.RecentHistory(u, 10); len(hist) != 0 {
		t.Errorf("prune kept %d records", len(hist))
	}
}

func TestMigrationsAreRecordedAndIdempotent(t *testing.T) {
	st, path := open(t)
	if _, err := st.CreateEvent(u, dentist); err != nil {
		t.Fatal(err)
	}
	st.Close()

	again, err := store.Open(path) // re-running migrations must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if list, _ := again.ListEvents(u); len(list) != 1 {
		t.Errorf("data lost on reopen: %d events", len(list))
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 1 {
		t.Errorf("user_version = %d, %v", v, err)
	}
}

func TestOwnersAreSeparate(t *testing.T) {
	st, _ := open(t)
	mine, err := st.CreateEvent(u, dentist)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateEvent("bob", dentist); err != nil { // same name and date, other user: fine
		t.Fatalf("bob's identical event rejected: %v", err)
	}
	if _, err := st.GetEvent("bob", mine.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob got alice's event: %v", err)
	}
	if _, err := st.UpdateEvent("bob", mine.ID, dentist); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob updated alice's event: %v", err)
	}
	if err := st.DeleteEvent("bob", mine.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob deleted alice's event: %v", err)
	}
	// An import with alice's id gives bob a new event instead of taking hers over.
	res, err := st.Import("bob", []events.Event{{ID: mine.ID, Name: "Taken", Date: "2026-12-01", Reminders: []string{"1d"}}}, false)
	if err != nil || res.Created != 1 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	if got, _ := st.GetEvent(u, mine.ID); got.Name != "Dentist" {
		t.Errorf("alice's event was overwritten: %+v", got)
	}
	if all, _ := st.AllEvents(); len(all) != 3 {
		t.Errorf("AllEvents = %d, want 3", len(all))
	}
	if mineOnly, _ := st.ListEvents(u); len(mineOnly) != 1 || mineOnly[0].Owner != u {
		t.Errorf("ListEvents(alice) = %+v", mineOnly)
	}
}

func TestUsers(t *testing.T) {
	st, _ := open(t)
	if _, err := st.GetUser("alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
	created, err := st.CreateUser("alice", "tk_1")
	if err != nil || len(created.FeedToken) < 32 {
		t.Fatalf("CreateUser = %+v, %v", created, err)
	}
	if _, err := st.CreateUser("alice", "tk_2"); err == nil {
		t.Error("duplicate user created")
	}
	if err := st.SetNtfyToken("alice", "tk_3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.UserByFeedToken(created.FeedToken); got.Username != "alice" || got.NtfyToken != "tk_3" {
		t.Errorf("UserByFeedToken = %+v", got)
	}
	newToken, err := st.ResetFeedToken("alice")
	if err != nil || newToken == created.FeedToken {
		t.Fatalf("ResetFeedToken = %q, %v", newToken, err)
	}
	if _, err := st.UserByFeedToken(created.FeedToken); !errors.Is(err, store.ErrNotFound) {
		t.Error("old feed token still works")
	}
	st.CreateUser("bob", "tk_b")
	if names, _ := st.Usernames(); len(names) != 2 || names[0] != "alice" || names[1] != "bob" {
		t.Errorf("Usernames = %v", names)
	}
}

func TestSessions(t *testing.T) {
	st, _ := open(t)
	cookie, err := st.CreateSession("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if name, exp, err := st.Session(cookie); err != nil || name != "alice" || !exp.Equal(testutil.FixedNow.Add(time.Hour)) {
		t.Errorf("Session = %q %v %v", name, exp, err)
	}
	if _, _, err := st.Session("not-a-session"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown cookie: %v", err)
	}

	testutil.FreezeClock(t, testutil.FixedNow.Add(30*time.Minute))
	st.RenewSession(cookie, time.Hour)
	testutil.FreezeClock(t, testutil.FixedNow.Add(80*time.Minute))
	if _, _, err := st.Session(cookie); err != nil {
		t.Errorf("renewed session expired: %v", err)
	}
	testutil.FreezeClock(t, testutil.FixedNow.Add(2*time.Hour))
	if _, _, err := st.Session(cookie); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session still valid: %v", err)
	}
	st.PruneSessions()

	other, _ := st.CreateSession("alice", time.Hour)
	st.DeleteSession(other)
	if _, _, err := st.Session(other); !errors.Is(err, store.ErrNotFound) {
		t.Error("deleted session still valid")
	}
}
