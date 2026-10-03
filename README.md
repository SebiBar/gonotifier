# gonotifier

A small self-hosted reminder service. Add events such as birthdays, appointments or bills,
choose how long before each one you want to be reminded, and gonotifier sends push
notifications to your devices through [ntfy](https://ntfy.sh).

- **Web UI** to add, edit and delete events (works on mobile, light and dark mode)
- **One-time or repeating events**: daily, weekly, monthly or yearly, every N, optionally until a date
- **Several reminders per event**, e.g. 7 days, 1 day and 1 hour before
- **On time**: each reminder is sent the moment it's due, and missed ones are caught up after downtime
- **Calendar feed** to show your events in Google Calendar or any calendar app, optionally at a secret URL
- **JSON API** with import/export, so scripts, other services or AI tools can read and manage events
- **Lightweight**: one static Go binary and one SQLite file; a minimal image for `amd64` and `arm64` (Raspberry Pi)

## Quick start

gonotifier sends notifications through an [ntfy](https://docs.ntfy.sh/) server, so you need one it can reach.

```bash
docker run -d --name gonotifier -p 8080:8080 \
  -e NTFY_URL=https://ntfy.example.com \
  -e TZ=America/New_York \
  -v gonotifier-data:/data \
  ghcr.io/sebibar/gonotifier:latest
```

Open http://localhost:8080 and click **New event**.

## Docker Compose

```yaml
services:
  gonotifier:
    image: ghcr.io/sebibar/gonotifier:latest   # or pin a release, e.g. :1.0.0
    restart: unless-stopped
    environment:
      NTFY_URL: http://ntfy:80
      NTFY_TOKEN: ${NTFY_TOKEN}                # only if your ntfy server requires auth
      TZ: America/New_York
    volumes:
      - ./data:/data                           # your events — back this up
    ports:
      - "8080:8080"                            # leave out if a reverse proxy fronts it
```

## Configuration

All settings are environment variables. Only `NTFY_URL` is required.

| Variable | Default | Description |
|---|---|---|
| `NTFY_URL` | **required** | ntfy server URL, e.g. `http://ntfy:80` |
| `NTFY_TOKEN` | — | ntfy access token, if your server requires auth |
| `NTFY_DEFAULT_TOPIC` | `reminders` | Topic used when an event doesn't set its own |
| `TZ` | `UTC` | Your timezone, e.g. `America/New_York` |
| `DEFAULT_NOTIFY_TIME` | `09:00` | When day-based reminders fire |
| `CATCHUP_WINDOW` | `24h` | How late a missed reminder may still be sent |
| `DB_PATH` | `/data/gonotifier.db` | SQLite database (events and sent reminders) |
| `EXPORT_FILE` | `events-export.json` next to the database | Read-only JSON copy of all events; `off` disables it |
| `FEED_TOKEN` | — | Serve the calendar feed only at `/feed/<token>.ics` (16+ characters of `A-Z a-z 0-9 - _`) |
| `PORT` | `8080` | HTTP port |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

## Events

| Field | Required | Description |
|---|---|---|
| `name` | yes | What to remind you about |
| `date` | yes | `YYYY-MM-DD` (all day) or `YYYY-MM-DDTHH:MM` (at a time). For repeats, the first occurrence |
| `reminders` | yes | When to notify, e.g. `["7d", "1d", "1h"]` |
| `repeat` | | `daily`, `weekly`, `monthly` or `yearly`; omit for a one-time event |
| `every` | | Repeat interval: `every: 3` with `monthly` means every 3 months |
| `until` | | Last date (`YYYY-MM-DD`) a repeat may fall on |
| `notify_time` | | When day-based reminders fire for this event, e.g. `08:00` |
| `topic` | | ntfy topic for this event (default `NTFY_DEFAULT_TOPIC`) |
| `priority` | | `min`, `low`, `default`, `high` or `urgent` |
| `tags` | | ntfy tags, comma-separated (replaces the automatic ones) |
| `auto_remove` | | `false` keeps the event after it has finished (default `true`) |

**Reminder offsets:** `Nd` fires N days before at the notify time (`0d` = 00:00 on the day);
`Nh` / `Nm` fire exactly that long before the event's time. On a repeating event, a reminder must
be shorter than the repeat interval.

**Repeats** keep the same day of the month; on the 31st they fall on the last day of shorter months.

**Auto-remove:** a finished event (a one-time event that has passed, or a repeat past its `until`
date) is deleted once all its reminders have been sent.

## API

Other services can manage events over HTTP, using the fields above plus a server-assigned `id`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/events` | List events, each with its `id` and `next` occurrence |
| `POST` | `/api/events` | Create an event; returns `201`, or `400` with validation errors |
| `GET` | `/api/events/{id}` | Get one event |
| `PUT` | `/api/events/{id}` | Replace an event (keeps its `id`) |
| `DELETE` | `/api/events/{id}` | Delete an event; returns `204`, or `404` |
| `GET` | `/api/export` | All events as `{"events": [...]}` |
| `POST` | `/api/import` | Add or update events from an export (or a list of events) |
| `GET` | `/health` | `{"status":"ok","events":N,"last_check":"…","next_reminder":"…"}` |

```bash
curl -X POST http://gonotifier:8080/api/events \
  -H "Content-Type: application/json" \
  -d '{"name":"Server maintenance","date":"2026-12-15T02:00","reminders":["1d","1h"]}'
```

**Import/export:** an import updates events whose `id` matches and creates the rest; add
`?mode=replace` to also delete events missing from the file. If any event is invalid, nothing changes.

```bash
curl http://gonotifier:8080/api/export > events.json
curl --data-binary @events.json http://gonotifier:8080/api/import
```

## Calendar feed

The feed lists every event with its repeats and reminders, at `/feed.ics`. Anyone who can reach that
URL can read your event names, so if the feed is reachable from the internet, set a secret token:

```bash
openssl rand -hex 24          # use the output as FEED_TOKEN
```

The feed then lives at `/feed/<token>.ics`, and `/feed.ics` returns 404. The **Calendar feed** link
in the web UI always points to the right URL.

To subscribe in Google Calendar: **Settings → Add calendar → From URL**, and paste
`https://<your-gonotifier-host>` followed by the feed path. The URL must be reachable from the
internet; Google refreshes it every 12–24 hours.

## Data and backups

Everything lives in `/data`: `gonotifier.db` holds your events, and `events-export.json` is a readable
copy rewritten on every change. Back up the export file (copying the database while the app runs can
catch it mid-write); restore it with the import command above. Mount a **directory** at `/data`, not
a single file.

## Security

gonotifier has **no login**, and neither does its API. Keep it on a private network or behind a
reverse proxy with authentication. Only the calendar feed needs to be public, if a calendar service
fetches it; set `FEED_TOKEN` and expose just the `/feed/` path.

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for how it works inside, running it locally, the project layout,
and releases.

## License

[MIT](LICENSE)
