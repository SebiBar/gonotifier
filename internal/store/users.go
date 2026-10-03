package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/sebibar/gonotifier/internal/clock"
)

// User is someone who has logged in at least once. Their password lives in ntfy, not here.
type User struct {
	Username  string
	NtfyToken string // sends this user's reminders
	FeedToken string // the calendar feed is served at /feed/<FeedToken>.ics
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GetUser returns a user, or ErrNotFound.
func (s *Store) GetUser(username string) (User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT username, ntfy_token, feed_token FROM users WHERE username = ?`, username))
}

// UserByFeedToken returns the user whose calendar feed has this token, or ErrNotFound.
func (s *Store) UserByFeedToken(token string) (User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT username, ntfy_token, feed_token FROM users WHERE feed_token = ?`, token))
}

func (s *Store) scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.Username, &u.NtfyToken, &u.FeedToken)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// Usernames returns every user, sorted.
func (s *Store) Usernames() ([]string, error) {
	rows, err := s.db.Query(`SELECT username FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CreateUser adds a user with a fresh feed token. The very first user also takes over
// events and history from before gonotifier had users (empty owner).
func (s *Store) CreateUser(username, ntfyToken string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := User{Username: username, NtfyToken: ntfyToken, FeedToken: randomToken()}
	tx, err := s.db.Begin()
	if err != nil {
		return u, err
	}
	defer tx.Rollback()
	var others int
	if err := tx.QueryRow(`SELECT count(*) FROM users`).Scan(&others); err != nil {
		return u, err
	}
	if _, err := tx.Exec(`INSERT INTO users (username, ntfy_token, feed_token, created_at) VALUES (?, ?, ?, ?)`,
		u.Username, u.NtfyToken, u.FeedToken, clock.Now().UTC().Format(dbTimeFormat)); err != nil {
		return u, err
	}
	if others == 0 {
		for _, table := range []string{"events", "sent"} {
			if _, err := tx.Exec(`UPDATE `+table+` SET owner = ? WHERE owner = ''`, username); err != nil {
				return u, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return u, err
	}
	s.version.Add(1)
	return u, nil
}

// SetNtfyToken replaces the token used to send the user's reminders.
func (s *Store) SetNtfyToken(username, token string) error {
	_, err := s.db.Exec(`UPDATE users SET ntfy_token = ? WHERE username = ?`, token, username)
	return err
}

// ResetFeedToken gives the user a new calendar feed URL; the old one stops working.
func (s *Store) ResetFeedToken(username string) (string, error) {
	token := randomToken()
	_, err := s.db.Exec(`UPDATE users SET feed_token = ? WHERE username = ?`, token, username)
	return token, err
}

// ---------- sessions ----------

// Sessions are stored by the sha256 of the cookie value, so the database alone can't log anyone in.
func sessionID(cookie string) string {
	h := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(h[:])
}

// CreateSession starts a session for the user and returns the cookie value.
func (s *Store) CreateSession(username string, ttl time.Duration) (string, error) {
	cookie := randomToken()
	_, err := s.db.Exec(`INSERT INTO sessions (id, username, expires_at) VALUES (?, ?, ?)`,
		sessionID(cookie), username, clock.Now().Add(ttl).UTC().Format(dbTimeFormat))
	return cookie, err
}

// Session returns the user and expiry of an unexpired session, or ErrNotFound.
func (s *Store) Session(cookie string) (string, time.Time, error) {
	var username, expires string
	err := s.db.QueryRow(`SELECT username, expires_at FROM sessions WHERE id = ? AND expires_at > ?`,
		sessionID(cookie), clock.Now().UTC().Format(dbTimeFormat)).Scan(&username, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	t, _ := time.Parse(dbTimeFormat, expires)
	return username, t, err
}

// RenewSession moves the session's expiry to ttl from now.
func (s *Store) RenewSession(cookie string, ttl time.Duration) error {
	_, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`,
		clock.Now().Add(ttl).UTC().Format(dbTimeFormat), sessionID(cookie))
	return err
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(cookie string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID(cookie))
	return err
}

// PruneSessions removes expired sessions.
func (s *Store) PruneSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, clock.Now().UTC().Format(dbTimeFormat))
	return err
}
