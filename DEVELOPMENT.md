# Development

Technical notes for working on gonotifier. For using it, see the [README](README.md).

## Guidelines

- **Simple over clever.** Prefer the plain solution (e.g. a fixed 5-minute retry over back-off). Add a
  feature only when it's needed, and remove code that no longer earns its place.
- **Small footprint.** It runs on a Raspberry Pi: one static binary (`CGO_ENABLED=0`), no polling
  loops besides the scheduler, no extra services, and nothing loaded from the internet at runtime.
- **Few dependencies.** Use the standard library first. A new dependency needs a clear reason, and
  the frontend gets no build step beyond `go tool templ generate`.
- **Time:** use `clock.Now()` (never `time.Now()`) in app logic, so tests can freeze time, and
  always the configured `TZ`.
- **Data:** change the schema only by appending a migration. Validation lives in `events.Validate`,
  and only `store` talks to SQLite.
- **API:** JSON only. Other containers depend on it, so don't rename or remove fields.
- **UI:** server-rendered (templ + htmx, a little Alpine.js), no emojis, and it must work on mobile and in
  light and dark mode. Pass values to Alpine.js through JSON (`alpineState`), never string concatenation.
- **Before committing:** `go tool templ generate`, `gofmt`, `go vet ./...` and `go test ./...`. Behavior
  changes come with a test next to the code. Update README.md (users) or this file (internals) to match.

## How it works

**Users** are ntfy's users. The login form (and the API's `Authorization` header) is checked by
calling ntfy's `GET /v1/account`; gonotifier stores no passwords. On a user's first login it creates
an ntfy token for them (`POST /v1/account/token`, never expires) and keeps it in the `users` table,
along with their calendar feed token. The web UI then uses a session cookie (30 days, renewed while
in use, re-checked with ntfy once a day). Events and sent reminders have an `owner`, and every store
query is scoped by it.

Events live in a SQLite database. The scheduler works out when the next reminder is due and sleeps
until exactly then; adding, editing or deleting an event wakes it to re-plan immediately. It also
re-plans at least once a minute, which absorbs wall-clock jumps (NTP sync after boot on a Raspberry
Pi without a hardware clock, DST changes, suspend).

Each reminder is sent to ntfy once, with its owner's token, and recorded in the `sent` table, keyed by event ID, offset and
occurrence date, so restarts never cause duplicates and each occurrence of a repeat is reminded
afresh. Event IDs are random and permanent, so editing an event never re-sends a reminder.

- **Catch-up:** reminders missed while the server was down are still sent if they are at most
  `CATCHUP_WINDOW` late. A reminder sent more than 10 minutes late says so:
  "Team standup was yesterday at 09:30 (delayed reminder)".
- **Retries:** if ntfy can't be reached, a send is retried every 5 minutes.
- **Housekeeping (hourly):** finished events are auto-removed, sent records older than a year
  are pruned, and so are expired sessions.
- **Export:** `exports/<username>.json` (the same format as `/api/export`) is rewritten for every
  user after every change.

## Running locally

Requires Go 1.26 or newer. Nothing else needs installing: templ is pinned in `go.mod` as a Go tool.

```bash
go test ./...
```

Running the app needs an ntfy server to log in to. Start one with a user, then gonotifier:

```bash
docker run -d --name ntfy -p 2586:80 -e NTFY_AUTH_FILE=/tmp/auth.db -e NTFY_AUTH_DEFAULT_ACCESS=deny-all \
  binwiederhier/ntfy serve
docker exec -e NTFY_PASSWORD=dev-pw ntfy ntfy user add dev
docker exec ntfy ntfy access dev 'dev_*' rw
NTFY_URL=http://localhost:2586 DB_PATH=./data/gonotifier.db go run ./cmd/gonotifier
```

Then open http://localhost:8080 and log in as `dev` / `dev-pw`. The `data/` folder is gitignored.
Reminders go to the topic `dev_reminders`.

To see which reminders are due right now without sending anything:

```bash
NTFY_URL=http://localhost:2586 DB_PATH=./data/gonotifier.db go run ./cmd/gonotifier -dry-run
docker exec gonotifier /usr/local/bin/gonotifier -dry-run   # in a running container
```

## Project layout

```
cmd/gonotifier/        entry point: loads config, starts the scheduler and web server
internal/
  config/              environment-variable settings
  events/              Event type, repeats and reminder maths, validation, import/export format
  store/               SQLite: users and sessions, events, sent-reminder log, schema migrations
  scheduler/           sleeps until the next reminder, sends it, removes finished events
  notify/              ntfy client (sending, login checks, tokens) and notification wording
  ical/                calendar feed
  web/                 HTTP handlers: login (auth.go), UI (ui.go), API (api.go), feed and health (web.go)
    *.templ            UI components: layout, event list, add/edit form (*_templ.go generated)
    static/            app.css, favicon, vendor/ (pinned third-party files)
  clock/               current time, replaceable in tests
  testutil/            shared test helpers (temp database, fake ntfy server with accounts, frozen clock)
scripts/vendor.sh      downloads the pinned frontend libraries
```

Tests live next to the code they test (`*_test.go`), as is conventional in Go.

## Editing the UI

The HTML lives in [templ](https://templ.guide) components (`internal/web/*.templ`): HTML with Go
expressions, checked by the compiler. After changing a `.templ` file, regenerate the Go code:

```bash
go tool templ generate
```

The generated `*_templ.go` files are committed, so building never needs templ; CI fails if they're
out of date. Interactivity comes from htmx (server round-trips) and a little Alpine.js (showing and
hiding parts of the form). Styling is in `internal/web/static/app.css`, on top of Pico CSS.

## Frontend libraries

htmx, Alpine.js, Pico CSS and the Inter font are committed in `internal/web/static/vendor/` and
embedded in the binary, so the UI never depends on a CDN. The pinned versions are listed in
`internal/web/static/vendor/VERSIONS`. To upgrade, change the versions at the top of
`scripts/vendor.sh`, run it on your machine, and click through the app before committing:

```bash
sh scripts/vendor.sh
```

## Database changes

Schema changes go in `internal/store/store.go` as a new entry at the end of `migrations`; they run
automatically on startup. Never edit or reorder existing entries.

## Container image

The runtime image is [distroless](https://github.com/GoogleContainerTools/distroless) `static`: only
the binary, CA certificates and timezone data, with no shell or package manager. The Docker health
check runs `gonotifier -healthcheck`, which calls `/health` on the running server.

Without a shell, debug from outside the container:

- Logs: `docker logs gonotifier` (set `LOG_LEVEL=debug` for more detail)
- Data: `gonotifier.db` and `exports/` are in the directory mounted at `/data`
- Network: start a separate throwaway container that has a shell (here the small `alpine` Linux
  image) on gonotifier's network, and test from there:

  ```bash
  docker run --rm -it --network container:gonotifier alpine sh
  ```

The container runs as root so it can write to any mounted directory; set `user: "1000:1000"` in
compose to run as your own user instead (that user must be able to write to `/data`).

## Builds and releases

GitHub Actions checks the generated templ code and runs the tests on every push and pull request.
Pushes to `main` publish `ghcr.io/sebibar/gonotifier:latest`. A version tag publishes a matching
image tag:

```bash
git tag v1.0.0 && git push --tags      # publishes :1.0.0 and :1.0
```

The Dockerfile cross-compiles for each architecture, so the multi-arch build needs no emulation.
To build locally:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t gonotifier .
```
