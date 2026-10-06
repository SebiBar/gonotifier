# gonotifier

A small self-hosted reminder service: add events such as birthdays, appointments or bills, choose
when to be reminded, and gonotifier sends push notifications through [ntfy](https://ntfy.sh).
Several users, repeating events, a calendar feed and a JSON API; one Go binary and one SQLite file,
with images for `amd64` and `arm64` (Raspberry Pi).

## Quick start

gonotifier's users are your ntfy server's users: you log in with your ntfy username and password.
So you need an ntfy server with [access control](https://docs.ntfy.sh/config/#access-control) and
at least one user.

```yaml
services:
  gonotifier:
    image: ghcr.io/sebibar/gonotifier:latest
    restart: unless-stopped
    environment:
      NTFY_URL: http://ntfy:80
      TZ: America/New_York
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
```

On your first login gonotifier creates an ntfy token for you (labelled `gonotifier`) and sends your
reminders with it, by default to the topic `<username>_reminders`. So give each user write access
to their own topics, e.g. `ntfy access alice 'alice_*' rw`.

## Configuration

Environment variables; only `NTFY_URL` is required.

| Variable | Default | Description |
|---|---|---|
| `NTFY_URL` | **required** | ntfy server URL, e.g. `http://ntfy:80`. Logins are checked there too |
| `TZ` | `UTC` | Your timezone, e.g. `America/New_York` |
| `DAY_START` | `00:00` | When all-day events start; their reminders count back from it |
| `CATCHUP_WINDOW` | `24h` | How late a missed reminder (e.g. after downtime) may still be sent |
| `DB_PATH` | `/data/gonotifier.db` | SQLite database |
| `FEED_URL` | — | Public address of the calendar feed, e.g. `https://cal.example.com`, if it differs from the UI's |
| `EXPORT_DIR` | `exports` next to the database | Readable JSON copy of each user's events; `off` disables it |
| `PORT` | `8080` | HTTP port |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

## Events

| Field | Required | Description |
|---|---|---|
| `name` | yes | What to remind you about |
| `date` | yes | `YYYY-MM-DD` (all day) or `YYYY-MM-DDTHH:MM`. For repeats, the first occurrence |
| `reminders` | yes | When to notify, e.g. `["1d", "1h", "0m"]` (up to 10) |
| `repeat` | | `daily`, `weekly`, `monthly` or `yearly`; omit for a one-time event |
| `every` | | Repeat interval: `3` with `monthly` means every 3 months |
| `until` | | Last date (`YYYY-MM-DD`) a repeat may fall on |
| `day_start` | | All-day events: overrides `DAY_START`, e.g. `08:00` |
| `topic` | | ntfy topic (default `<username>_reminders`); shared topics work too |
| `priority` | | ntfy priority: `min`, `low`, `default`, `high` or `urgent` |
| `tags` | | ntfy tags, comma-separated (replace the automatic ones) |
| `auto_remove` | | `false` keeps the event once it has finished (default `true`) |

**Reminders** count back from the event's start: its time, or for an all-day event its day start.
`30m` and `2h` are exact; `1d` and `7d` are calendar days, so they keep the clock time across DST.
`0m` fires on time. On a repeating event, each reminder must be shorter than the repeat interval.

When you create an event or change its date or reminders, nothing may already be in the past: not the
event, nor a reminder that would already have been sent. Imports skip this, so backups can be restored.

Monthly repeats on the 31st fall on the last day of shorter months. A finished event (a one-time
event that passed, or a repeat past `until`) is deleted once its reminders are sent, unless
`auto_remove` is `false`.

## API

Requests use the same credentials ntfy accepts: an ntfy token (`Authorization: Bearer tk_…`) or the
username and password. Each user only sees their own events.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/events` | Your events, each with its `id` and `next` occurrence |
| `POST` | `/api/events` | Create an event; `201`, or `400` with validation errors |
| `GET` `PUT` `DELETE` | `/api/events/{id}` | Get, replace (keeps the `id`) or delete one event |
| `GET` | `/api/export` | All your events as `{"events": [...]}` |
| `POST` | `/api/import` | Add or update events by `id`; `?mode=replace` also deletes the rest. All or nothing |
| `GET` | `/health` | Status, event count, last check and next reminder (no login) |

```bash
curl -X POST http://gonotifier:8080/api/events -H "Authorization: Bearer tk_..." \
  -d '{"name":"Server maintenance","date":"2026-12-15T02:00","reminders":["1d","1h"]}'
```

## Calendar feed

Each user's events are also a calendar feed at a secret URL: copy it from **Calendar feed** in the UI,
and replace it with **New link** if it leaks. Add it to your calendar app as a subscription (Google
Calendar: **Other calendars → + → From URL**); opening the `.ics` file instead imports a copy that
never updates. Google refreshes subscriptions every 12–24 hours and ignores their reminders; other
apps (Apple Calendar, ICSx⁵ on Android) use them.

## Data and backups

`/data` holds `gonotifier.db` and `exports/<username>.json`, a copy of each user's events rewritten on
every change. Back up the database while gonotifier is stopped, or the export files at any time; a
user restores theirs through `/api/import`.

## Security

- **Logins** are checked by ntfy; gonotifier stores no passwords. A username is locked for 15 minutes
  after 5 wrong passwords. Sessions last 30 days while in use and are re-checked with ntfy daily.
- **Anyone with an ntfy account** on the server can log in. Users only see their own events, and
  reminders are sent with their own token, so ntfy's access rules decide where they can post.
- **Only the calendar feed** needs to be reachable from the internet (for Google Calendar). Expose
  just `/feed/` and keep the rest on your network or VPN.

Development: [DEVELOPMENT.md](DEVELOPMENT.md). License: [MIT](LICENSE).
