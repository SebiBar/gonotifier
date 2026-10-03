// Package notify sends push notifications through ntfy and formats reminder text.
package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sebibar/gonotifier/internal/events"
)

// Ntfy is a minimal client for an ntfy server.
type Ntfy struct {
	URL   string // e.g. http://ntfy:80
	Token string // optional access token
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// ErrUnauthorized means ntfy rejected the credentials.
var ErrUnauthorized = errors.New("wrong username, password or token")

// Account returns the username that the Authorization header ("Basic …" or "Bearer tk_…")
// belongs to, or ErrUnauthorized.
func (n Ntfy) Account(authorization string) (string, error) {
	var acc struct {
		Username string `json:"username"`
	}
	if err := n.account(http.MethodGet, "/v1/account", authorization, nil, &acc); err != nil {
		return "", err
	}
	if acc.Username == "" || acc.Username == "*" { // "*" is ntfy's anonymous user
		return "", ErrUnauthorized
	}
	return acc.Username, nil
}

// CreateToken creates a token that never expires for the user the Authorization header belongs to.
func (n Ntfy) CreateToken(authorization string) (string, error) {
	var tok struct {
		Token string `json:"token"`
	}
	body := strings.NewReader(`{"label":"gonotifier","expires":0}`)
	if err := n.account(http.MethodPost, "/v1/account/token", authorization, body, &tok); err != nil {
		return "", err
	}
	if tok.Token == "" {
		return "", errors.New("ntfy returned no token")
	}
	return tok.Token, nil
}

func (n Ntfy) account(method, path, authorization string, body io.Reader, out any) error {
	if authorization == "" {
		return ErrUnauthorized
	}
	req, err := http.NewRequest(method, strings.TrimRight(n.URL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode/100 != 2:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

var reTopicUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// DefaultTopic is where a user's reminders go when an event doesn't set a topic: <username>_reminders.
func DefaultTopic(username string) string {
	name := reTopicUnsafe.ReplaceAllString(username, "_") // ntfy usernames may contain . + @
	if len(name) > 54 {
		name = name[:54] // topics are at most 64 characters
	}
	return name + "_reminders"
}

// Send POSTs a message to {URL}/{topic} with Title, Priority and Tags headers.
func (n Ntfy) Send(topic, title, message, priority, tags string) error {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(n.URL, "/")+"/"+topic, strings.NewReader(message))
	if err != nil {
		return err
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	// RFC 2047 encoding keeps non-ASCII text intact through HTTP headers; ntfy decodes it.
	req.Header.Set("Title", mime.BEncoding.Encode("utf-8", title))
	if priority != "" {
		req.Header.Set("Priority", priority)
	}
	if tags != "" {
		req.Header.Set("Tags", tags)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

var reBirthdayOwner = regexp.MustCompile(`(?i)^(.+?)['’]s? birthday$`)

// Format generates (title, message, tags) for a reminder.
//
//	Yearly in 5d     → ("Birthday Reminder", "Mom's birthday is in 5 days (March 15)", "birthday,cake")
//	Yearly tmrw      → ("Birthday Reminder", "Mom's birthday is tomorrow!", "birthday,cake")
//	Yearly today     → ("Happy Birthday!", "Happy birthday Mom!", "birthday,tada")
//	Timed in 12h     → ("Reminder", "Dentist in 12 hours (Nov 15 at 10:00)", "calendar")
//	Date-only today  → ("Reminder", "Renew car insurance is today!", "bell")
//
// Yearly events whose name doesn't mention "birthday" get generic annual wording; other
// repeats are worded like one-time events. eventDatetime is the occurrence being reminded of.
func Format(event events.Event, offset string, eventDatetime time.Time) (title, message, tags string) {
	dur, unit, err := events.ParseOffset(offset)
	if err != nil {
		return "Reminder", event.Name, "bell"
	}
	days := int(dur / (24 * time.Hour))
	isToday := unit == "d" && days == 0
	isTomorrow := unit == "d" && days == 1
	when := events.HumanOffset(dur, unit)

	switch {
	case event.Repeat == events.Yearly:
		dateStr := eventDatetime.Format("January 2")
		birthday := strings.Contains(strings.ToLower(event.Name), "birthday")
		if birthday {
			title, tags = "Birthday Reminder", "birthday,cake"
		} else {
			title, tags = "Annual Reminder", "repeat"
		}
		switch {
		case isToday && birthday:
			who := event.Name
			if m := reBirthdayOwner.FindStringSubmatch(event.Name); m != nil {
				who = m[1]
			}
			return "Happy Birthday!", fmt.Sprintf("Happy birthday %s!", who), "birthday,tada"
		case isToday:
			return "Annual Reminder", fmt.Sprintf("%s is today!", event.Name), "tada"
		case isTomorrow:
			message = fmt.Sprintf("%s is tomorrow!", event.Name)
		default:
			message = fmt.Sprintf("%s is in %s (%s)", event.Name, when, dateStr)
		}
		return title, message, tags

	case event.HasTime():
		clock := eventDatetime.Format("15:04")
		switch {
		case isToday:
			message = fmt.Sprintf("%s is today at %s", event.Name, clock)
		case isTomorrow:
			message = fmt.Sprintf("%s is tomorrow at %s", event.Name, clock)
		default:
			message = fmt.Sprintf("%s in %s (%s at %s)", event.Name, when, eventDatetime.Format("Jan 2"), clock)
		}
		return "Reminder", message, "calendar"

	default:
		switch {
		case isToday:
			message = fmt.Sprintf("%s is today!", event.Name)
		case isTomorrow:
			message = fmt.Sprintf("%s is tomorrow (%s)", event.Name, eventDatetime.Format("Jan 2"))
		default:
			message = fmt.Sprintf("%s is in %s (%s)", event.Name, when, eventDatetime.Format("Jan 2"))
		}
		return "Reminder", message, "bell"
	}
}

// FormatLate words a reminder that is going out well after its fire time (e.g. after
// downtime). Offset-based wording like "in 10 minutes" would be wrong by then, so the
// message describes the occurrence relative to now and says the reminder was delayed:
//
//	"Team standup was yesterday at 09:30 (delayed reminder)"
//	"Dentist is tomorrow at 10:00 (delayed reminder)"
//	"Renew car insurance is today! (delayed reminder)"
func FormatLate(event events.Event, occurrence, now time.Time) string {
	days := calendarDays(now, occurrence)
	var when string
	if event.HasTime() {
		clock := occurrence.Format("15:04")
		past := occurrence.Before(now)
		switch {
		case days == 0 && past:
			when = "was today at " + clock
		case days == 0:
			when = "is today at " + clock
		case days == -1:
			when = "was yesterday at " + clock
		case days == 1:
			when = "is tomorrow at " + clock
		case days < 0:
			when = fmt.Sprintf("was on %s at %s", occurrence.Format("Jan 2"), clock)
		default:
			when = fmt.Sprintf("is in %d days (%s at %s)", days, occurrence.Format("Jan 2"), clock)
		}
	} else {
		switch {
		case days == 0:
			when = "is today!"
		case days == -1:
			when = "was yesterday"
		case days == 1:
			when = fmt.Sprintf("is tomorrow (%s)", occurrence.Format("Jan 2"))
		case days < 0:
			when = "was on " + occurrence.Format("Jan 2")
		default:
			when = fmt.Sprintf("is in %d days (%s)", days, occurrence.Format("Jan 2"))
		}
	}
	return event.Name + " " + when + " (delayed reminder)"
}

// calendarDays counts calendar days from a to b, ignoring the time of day.
func calendarDays(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.In(a.Location()).Date()
	return int(time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)) / (24 * time.Hour))
}
