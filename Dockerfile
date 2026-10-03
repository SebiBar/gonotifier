# syntax=docker/dockerfile:1

# Build stage runs natively on the build machine ($BUILDPLATFORM) and cross-compiles
# for each target platform. The app is pure Go (no CGo), so no CPU emulation is needed.
# Generated templ code (*_templ.go) is committed, so a plain go build is enough.
# This golang:*-alpine stage only compiles; it is discarded and not part of the final image.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/gonotifier ./cmd/gonotifier \
 && mkdir -p /out/data

# Runtime: distroless static — no shell or package manager, just CA certificates and
# timezone data, so there is almost nothing for vulnerability scanners to flag.
# It runs as root so bind-mounted host directories stay writable; set `user:` in
# compose to run as your own UID instead.
FROM gcr.io/distroless/static-debian13
COPY --from=builder /out/gonotifier /usr/local/bin/gonotifier
# The SQLite database and the events export live here; mount a volume or directory on it.
COPY --from=builder /out/data /data
EXPOSE 8080
HEALTHCHECK --interval=60s --timeout=5s --retries=3 CMD ["/usr/local/bin/gonotifier", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/gonotifier"]
