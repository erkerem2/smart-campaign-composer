# Smart Campaign Composer - backend images (ADR 011).
#
# One Dockerfile, one compile, three small runtime images:
#
#   docker build --target server        ./backend   # HTTP API on :8080 (default target)
#   docker build --target seed          ./backend   # one-shot seeder (3,000 users)
#   docker build --target mock-provider ./backend   # mock SMS / e-mail gateway on :9090
#   docker build --target test          ./backend   # runs `go test ./...` inside the build
#
# The SQLite driver is pure Go (modernc.org/sqlite, ADR 002), so the binaries
# are built with CGO_ENABLED=0 and need no C toolchain or libc at runtime.

ARG GO_VERSION=1.24
ARG ALPINE_VERSION=3.20

# ---------------------------------------------------------------- toolchain --
FROM golang:${GO_VERSION}-alpine AS deps
WORKDIR /src
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local
# Dependencies first: this layer is reused until go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# -------------------------------------------------------------------- tests --
# `make test-docker` - the same suite as `make test`, with the mock LLM and an
# in-process mock provider; no API key and no network beyond the module proxy.
FROM deps AS test
COPY . .
RUN go test -count=1 ./...

# -------------------------------------------------------------------- build --
FROM deps AS build
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/server ./cmd/seed ./cmd/mock-provider

# ------------------------------------------------------------ runtime base --
# Alpine rather than distroless: busybox `wget` gives docker compose a real
# HTTP healthcheck without shipping a curl or an extra probe binary.
#
# No `apk add`: the build must not depend on the Alpine package CDN.
#  - TLS roots (api.anthropic.com) come from ca-certificates-bundle, which is
#    part of the alpine base image (/etc/ssl/certs/ca-certificates.crt).
#  - No tzdata: the code works in UTC only (ADR 003) and never calls
#    time.LoadLocation; Go resolves TZ=UTC without zoneinfo files.
#  - addgroup / adduser / mkdir / chown are busybox built-ins.
FROM alpine:${ALPINE_VERSION} AS runtime
RUN addgroup -S -g 10001 app \
 && adduser -S -D -H -u 10001 -G app app \
 && mkdir -p /data \
 && chown app:app /data
# /data is owned by the unprivileged user, so a fresh named volume mounted
# there inherits that ownership and the seeder / server can write the DB.
WORKDIR /app
ENV DB_PATH=/data/campaign.db \
    TZ=UTC
USER app:app

# ------------------------------------------------------------ mock provider --
FROM runtime AS mock-provider
COPY --from=build /out/mock-provider /app/mock-provider
ENV MOCK_PROVIDER_ADDR=:9090
EXPOSE 9090
ENTRYPOINT ["/app/mock-provider"]

# --------------------------------------------------------------------- seed --
# Idempotent: skips when users exist. Reseed with `-reset`
# (docker compose run --rm seed -reset).
FROM runtime AS seed
COPY --from=build /out/seed /app/seed
ENTRYPOINT ["/app/seed"]

# ------------------------------------------------------------------- server --
# Last stage = default target of `docker build ./backend`.
FROM runtime AS server
COPY --from=build /out/server /app/server
ENV HTTP_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/app/server"]
