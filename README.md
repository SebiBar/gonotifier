# gonotifier

A small self-hosted reminder service. Add events such as birthdays, appointments or bills,
choose how long before each one you want to be reminded, and gonotifier sends push
notifications to your devices through [ntfy](https://ntfy.sh).

- **Web UI** to add, edit and delete events (works on mobile, light and dark mode)
- **Several users**, each with their own events and reminders, logging in with their ntfy account
- **One-time or repeating events**: daily, weekly, monthly or yearly, every N, optionally until a date
- **Several reminders per event**, e.g. 7 days, 1 day and 1 hour before
- **On time**: each reminder is sent the moment it's due, and missed ones are caught up after downtime
- **Calendar feed** to show your events and their reminders in Google Calendar or any calendar app
- **JSON API** with import/export, so scripts, other services or AI tools can read and manage events
- **Lightweight**: one static Go binary and one SQLite file; a minimal image for `amd64` and `arm64` (Raspberry Pi)

## Quick start

gonotifier sends notifications through an [ntfy](https://docs.ntfy.sh/) server, and its users are
that server's users: you log in with your ntfy username and password. So you need an ntfy server
with [access control](https://docs.ntfy.sh/config/#access-control) and at least one user.

```yaml
services:
  gonotifier:
    image: ghcr.io/sebibar/gonotifier:latest   # or pin a release, e.g. :0.2
    restart: unless-stopped
    environment:
      NTFY_URL: http://ntfy:80
      TZ: America/New_York
    volumes:
      - ./data:/data                           # your events — back this up
    ports:
      - "8080:8080"                            # leave out if a reverse proxy fronts it
```

Open http://localhost:8080, log in with your ntfy account and click **New event**.

On your first login gonotifier creates an ntfy token for you (labelled `gonotifier`), and sends
your reminders with it. By default they go to the topic `<username>_reminders`, so give each user
write access to their own topics, e.g. `ntfy access alice 'alice_*' rw`.

## Configuration

All settings are environment variables. Only `NTFY_URL` is required.

| Variable | Default | Description |
|---|---|---|
| `NTFY_URL` | **required** | ntfy server URL, e.g. `http://ntfy:80`. Logins are checked there too |
| `TZ` | `UTC` | Your timezone, e.g. `America/New_York` |
| `DEFAULT_NOTIFY_TIME` | `09:00` | When reminders of all-day events fire |
| `CATCHUP_WINDOW` | `24h` | How late a missed reminder may still be sent |
| `DB_PATH` | `/data/gonotifier.db` | SQLite database (events and sent reminders) |
| `FEED_URL` | — | Public address the calendar feed is reachable on, e.g. `https://cal.example.com`, if it differs from the one you open the UI on. The UI then shows feed links there |
| `EXPORT_DIR` | `exports` next to the database | Read-only JSON copy of each user's events, `<username>.json`; `off` disables it |
| `PORT` | `8080` | HTTP port |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

## Events

| Field | Required | Description |
|---|---|---|
| `name` | yes | What to remind you about |
| `date` | yes | `YYYY-MM-DD` (all day) or `YYYY-MM-DDTHH:MM` (at a time). For repeats, the first occurrence |
| `reminders` | yes | When to notify, e.g. `["1d", "1h", "0m"]` (see below) |
| `repeat` | | `daily`, `weekly`, `monthly` or `yearly`; omit for a one-time event |
| `every` | | Repeat interval: `every: 3` with `monthly` means every 3 months |
| `until` | | Last date (`YYYY-MM-DD`) a repeat may fall on |
| `notify_time` | | All-day events: when their reminders fire, e.g. `08:00` |
| `topic` | | ntfy topic for this event (default `<username>_reminders`). Shared topics work too, e.g. `family_reminders` |
| `priority` | | `min`, `low`, `default`, `high` or `urgent` |
| `tags` | | ntfy tags, comma-separated (replaces the automatic ones) |
| `auto_remove` | | `false` keeps the event after it has finished (default `true`) |

**Reminders** work like in calendar apps:
- **Events at a time:** `Nd`, `Nh` and `Nm` fire exactly that long before (`1d` = 24 hours before).
  `0m` fires at the event's time, for simple reminders like taking medicine.
- **All-day events:** `Nd` fires N days before, at the notify time (`0d` = on the day).

An event can have up to 10 reminders. On a repeating event, each must be shorter than the repeat
interval. When you create an event or change its date or reminders, nothing may already be in the past:
not the event itself, nor a reminder that would have been sent already. (Imports aren't checked, so a
backup with old events can be restored.)

**Repeats** keep the same day of the month; on the 31st they fall on the last day of shorter months.

**Auto-remove:** a finished event (a one-time event that has passed, or a repeat past its `until`
date) is deleted once all its reminders have been sent.

## API

Other services can manage events over HTTP, using the fields above plus a server-assigned `id`.
Requests are made as a user, with the same credentials ntfy accepts: an ntfy access token
(`Authorization: Bearer tk_…`) or the username and password. Each user only sees their own events.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/events` | List your events, each with its `id` and `next` occurrence |
| `POST` | `/api/events` | Create an event; returns `201`, or `400` with validation errors |
| `GET` | `/api/events/{id}` | Get one event |
| `PUT` | `/api/events/{id}` | Replace an event (keeps its `id`) |
| `DELETE` | `/api/events/{id}` | Delete an event; returns `204`, or `404` |
| `GET` | `/api/export` | All your events as `{"events": [...]}` |
| `POST` | `/api/import` | Add or update events from an export (or a list of events) |
| `GET` | `/health` | `{"status":"ok","events":N,"last_check":"…","next_reminder":"…"}` (no login needed) |

```bash
curl -X POST http://gonotifier:8080/api/events \
  -H "Authorization: Bearer tk_..." -H "Content-Type: application/json" \
  -d '{"name":"Server maintenance","date":"2026-12-15T02:00","reminders":["1d","1h"]}'
```

**Import/export:** an import updates events whose `id` matches and creates the rest; add
`?mode=replace` to also delete events missing from the file. If any event is invalid, nothing changes.

```bash
curl -u alice http://gonotifier:8080/api/export > events.json
curl -u alice --data-binary @events.json http://gonotifier:8080/api/import
```

## Calendar feed

Each user has a calendar feed with all their events, repeats and reminders, at a secret URL: copy it
from **Calendar feed** in the web UI. Calendar apps fetch it without logging in, so the URL is the
password: anyone who has it can read your event names. If it leaks, replace it with **New link**
(calendars subscribed to the old one then need the new one).

Add it to your calendar app as a subscription, so it stays up to date: in Google Calendar,
**Other calendars → + → From URL**. (Opening the `.ics` file instead imports a copy that never
updates.) The URL must be reachable from the internet; Google refreshes it every 12–24 hours.
Calendar apps decide for themselves whether to show a feed's reminders: Google Calendar ignores
them in subscriptions and uses your default notifications for that calendar. If only the feed is
public, on another hostname than the UI, set `FEED_URL` so the link already points there.

## Data and backups

Everything lives in `/data`: `gonotifier.db` holds users, events and sent reminders, and
`exports/<username>.json` is a readable copy of each user's events, rewritten on every change.
Back up the database while gonotifier is stopped, or the export files any time (copying the
database while the app runs can catch it mid-write). A user restores their file with the import
command above. Mount a **directory** at `/data`, not a single file.

## Security

- **Logins** are checked by ntfy; gonotifier stores no passwords. A username is locked for 15 minutes
  after 5 wrong passwords. Sessions last 30 days and are renewed while in use; once a day they check
  with ntfy that the user still exists.
- **The first user to log in** takes over events created before gonotifier had users (version 0.1).
- **Anyone with an ntfy account** on the server can log in. Each user only sees and changes their own
  events, and their reminders are sent with their own ntfy token, so ntfy's access rules decide which
  topics they can post to.
- **Exposing it to the internet:** only the calendar feed needs to be public, if a calendar service
  fetches it. Expose just the `/feed/` path and keep the rest on your network or VPN.

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for how it works inside, running it locally, the project layout,
and releases.

## License

[MIT](LICENSE)
