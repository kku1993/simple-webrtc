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

## Client (`client/`)

A TypeScript client wrapping [`simple-peer`](https://github.com/feross/simple-peer)
that speaks the protocol in `docs/DESIGN.md` against the Go server above. See
`client/AGENTS.md` for the full client notes; summary below.

### Layout

- `client/src/types.ts` — protocol message types, `CloseCode`, `ErrorCode`,
  renegotiation frame.
- `client/src/peer-connection.ts` — `PeerConnection`, the public wrapper that
  ties `simple-peer` to the protocol state machine.
- `client/src/transport.ts` — WebSocket wrapper (browser native + optional
  Node `ws`).
- `client/src/storage.ts` — `sessionStorage` persistence of
  `{ roomId, role, rejoinToken, hostEpoch, guestEpoch }`.
- `client/src/errors.ts` — `SignalingError` mapping `error-response` and close
  codes to the retry policy.
- `client/src/util.ts` — base64url epoch generation, sequence counter,
  full-jitter backoff.
- `client/src/emitter.ts` — minimal typed event emitter (no `events` dep).
- `client/src/index.ts` — public barrel.
- `client/test/` — `node:test` suite; `fakes.ts` provides a fake WebSocket and
  fake `simple-peer` so the state machine is exercised without `wrtc`.

### Build / test

```sh
cd client
npm install
npm install-scripts approve esbuild   # one-off: unblock tsx's postinstall
npm run build       # tsc -p tsconfig.build.json  -> dist/
npm run typecheck   # tsc -p tsconfig.json --noEmit (src + test)
npm run lint        # eslint .  (flat config, type-checked)
npm test            # node --test --import tsx test/*.test.ts
```

### Tooling notes

- TypeScript 5.9.3 (typescript-eslint 8.x does not yet support TS 7).
- ESLint 9.39.5 flat config (`eslint.config.js`); type-checked rules via
  `projectService`. `tsconfig.json` includes `src` + `test` (noEmit) for lint;
  `tsconfig.build.json` emits only `src`.
- Imports use the NodeNext `.js` extension convention.
- `simple-peer` is a CJS `export =` module; imported as
  `import SimplePeer from 'simple-peer'` (esModuleInterop).
- `ws` is an optional peer dependency (Node only); browsers use native
  `WebSocket`.
- `npm audit` flags a high-severity advisory in `brace-expansion` (transitive
  via eslint's `minimatch`); the fix requires eslint 10.8.0, published
  2026-07-24, which is within the 7-day freshness window and therefore pinned
  to 9.39.5. It is a dev-only transitive DoS in eslint's glob handling and does
  not affect the shipped library.

### Protocol behaviors implemented

- `createRoom` (host) / `joinRoom` (guest) / `rejoin` (recovery from a
  persisted session).
- Host waits for `guest-joined` before building the initiator `SimplePeer`;
  guest builds the non-initiator peer immediately on join.
- `signal` ↔ `signal-response` relay with per-(slot,epoch) `seq`.
- Sends `peer-connected` on `connect`; treats close `4200` (room-idle-close) as
  success — no reconnect.
- Renegotiation routed over the data channel as `{kind:'renegotiate', signal}`
  once the server has released the sockets.
- Reconnect with a fresh epoch + `rejoin-room` on retryable closes (`4008`,
  `4014`, `4300`); terminal handling for `4013` / `4400`.
- Symmetric epoch comparison: rebuilds `SimplePeer` when the peer's epoch
  changed (`peer-reset`), keeps it on `peer-rejoined`; roles/initiator never
  change.
- `SIGNAL_BUFFER_OVERFLOW` → rebuild with new epoch; `INVALID_REJOIN_TOKEN` /
  `ROOM_CLOSED` / `ROOM_EXPIRED` → terminal.
- `PeerConnection` accepts injectable `transportFactory` and
  `simplePeerFactory` options so the state machine is unit-testable without a
  browser or `wrtc`.
