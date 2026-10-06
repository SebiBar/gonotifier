# Development

Notes for working on gonotifier. For using it, see the [README](README.md).

## Guidelines

- **Simple over clever.** Prefer the plain solution (e.g. a fixed 5-minute retry over back-off). Add a
  feature only when it's needed, and remove code that no longer earns its place.
- **Follow common standards.** When in doubt, do what calendar apps and ntfy do.
- **One source for each rule.** Logic lives in one place and the rest uses it (e.g. the form's
  reminder choices come from `events.PeriodDays`, form and API save through `saveEvent`).
- **Small footprint.** It runs on a Raspberry Pi: one static binary (`CGO_ENABLED=0`), no extra
  services, and nothing loaded from the internet at runtime (frontend libraries are vendored; see
  `scripts/vendor.sh`).
- **Few dependencies.** Standard library first; a new dependency needs a clear reason. The frontend
  has no build step beyond `go tool templ generate`.
- **Packages:** date and reminder logic and validation live in `events` (no I/O); only `store` talks
  to SQLite.
- **Time:** use `clock.Now()` (never `time.Now()`) in app logic, so tests can freeze time, and always
  the configured `TZ`.
- **UI:** server-rendered (templ + htmx, a little Alpine.js), no emojis, works on mobile and in light
  and dark mode. Pass values to Alpine.js through JSON (`alpineState`), never string concatenation.
- **API:** JSON only. Other services use it, so renaming or removing a field is a breaking change.
- **Tests check behavior, not wording:** a test should fail when the app does the wrong thing, not
  when a label, message or placeholder changes. Behavior changes come with a test.
- **Before committing:** `sh scripts/check.sh` (templ generate, gofmt, vet, tests): the same checks CI runs. Commit the
  generated `*_templ.go` files with any `.templ` change; CI fails if they're out of date.
- **Docs:** keep the README (users) and this file up to date, with only what the code doesn't say.

## Logins

Users are ntfy's users. A login (or the API's `Authorization` header) is checked by calling ntfy's
`GET /v1/account`. On a user's first login gonotifier creates an ntfy token for them
(`POST /v1/account/token`, never expires) and sends their reminders with it, so ntfy's access
rules apply to what each user can post.

## Running locally

Requires Go 1.26+; templ is pinned in `go.mod` as a Go tool. Running the app needs an ntfy server
to log in to:

```bash
docker run -d --name ntfy -p 2586:80 -e NTFY_AUTH_FILE=/tmp/auth.db -e NTFY_AUTH_DEFAULT_ACCESS=deny-all \
  binwiederhier/ntfy serve
docker exec -e NTFY_PASSWORD=dev-pw ntfy ntfy user add dev
docker exec ntfy ntfy access dev 'dev_*' rw
NTFY_URL=http://localhost:2586 DB_PATH=./data/gonotifier.db go run ./cmd/gonotifier
```

Then open http://localhost:8080 and log in as `dev` / `dev-pw`. Add `-dry-run` to log which reminders
are due without sending anything (in a container: `docker exec gonotifier /usr/local/bin/gonotifier -dry-run`).

## Container

The image is [distroless](https://github.com/GoogleContainerTools/distroless): no shell. To debug,
use `docker logs gonotifier` (`LOG_LEVEL=debug` for more), or a throwaway container on its network:

```bash
docker run --rm -it --network container:gonotifier alpine sh
```

## Releases

Pushes to `main` publish `ghcr.io/sebibar/gonotifier:latest`; a version tag publishes matching tags:

```bash
git tag v1.0.0 && git push --tags
```
