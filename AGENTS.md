# simple-peer-signal-server

Go implementation of the signaling server in `docs/DESIGN.md`.

## Layout

- `src/cmd/server/` — entry point (`main`).
- `src/internal/config/` — env-based config + startup validation.
- `src/internal/protocol/` — wire types, error codes, close codes.
- `src/internal/token/` — signed rejoin tokens (HMAC-SHA256, constant-time verify).
- `src/internal/lru/` — generic bounded LRU cache.
- `src/internal/tombstone/` — bounded TTL LRU of deliberately-closed room IDs.
- `src/internal/ratelimit/` — token buckets + per-IP counter map (both bounded LRU).
- `src/internal/room/` — room registry, slot state machine, signal buffering,
  epochs, lifecycle timers, recreate-from-token.
- `src/internal/turnstile/` — Cloudflare Turnstile siteverify client.
- `src/internal/metrics/` — Prometheus instruments.
- `src/internal/server/` — WebSocket endpoint, origin check, handshake timeout,
  read/write loops, `/healthz`, `/metrics`.

## Build / test

```sh
cd src
go build ./...
go vet ./...
go test ./...            # all packages
go test -race ./...      # with the race detector
```

## Run

```sh
SERVER_SECRET="$(openssl rand -base64 32)" \
ALLOWED_ORIGINS="https://your.app" \
LISTEN_ADDR=":8080" \
go run ./cmd/server
```

`SERVER_SECRET` (>=32 bytes) and `ALLOWED_ORIGINS` are required; the server
refuses to start without them. Use `*` to disable origin checking (logs a
warning). See `docs/DESIGN.md` §"Configuration reference" for the full list.
