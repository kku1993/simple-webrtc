# simple-webrtc

Go implementation of the signaling server and typescript implementation of the
client in `docs/DESIGN.md`.

## Layout

- `src/cmd/server/` — entry point (`main`).
- `src/internal/config/` — env-based config + startup validation.
- `src/internal/protocol/` — wire types, error codes, close codes.
- `src/internal/token/` — signed rejoin tokens (HMAC-SHA256, constant-time verify).
- `src/internal/lru/` — generic bounded LRU cache.
- `src/internal/tombstone/` — bounded TTL LRU of deliberately-closed room IDs.
- `src/internal/ratelimit/` — token buckets + per-IP counter map (both bounded LRU).
- `src/internal/room/` — room registry, slot state machine, signal buffering,
  epochs, lifecycle timers, recreate-from-token, disk snapshot/restore
  (`snapshot.go`: versioned JSON projection of Room minus live Conn fields;
  `Restore` rehydrates rooms on startup, clearing `releaseAt` and setting a
  fresh peer deadline).
- `src/internal/roomid/` — room ID generator + validator following
  `docs/ROOM_ID_SPEC.md` (`[shard][nid]`, e.g. `ta0000`; Crockford base32
  fuzzy decoding on input).
- `src/internal/turnstile/` — Cloudflare Turnstile siteverify client.
- `src/internal/turn/` — Cloudflare Calls TURN credential client; mints
  short-lived `iceServers` arrays minted per handshake when `TURN_KEY_ID` /
  `TURN_KEY_API_TOKEN` are configured.
- `src/internal/metrics/` — Prometheus instruments.
- `src/internal/requestlog/` — JSON request logging (HTTP + WebSocket).
- `src/internal/server/` — WebSocket endpoint, origin check, handshake timeout,
  `/healthz`, `/metrics`, and the WebSocket transport: framing over gobwas/ws
  (`wsconn.go`), epoll-driven reads (`poller_linux.go`) with a
  goroutine-per-connection fallback (`poller_other.go`, `readloop.go`).
- `src/internal/state/` — disk persistence of room state. `FileStore` writes
  one JSON file per room (atomic temp+rename); `Persister` is a batching,
  coalescing write queue that drains a channel of dirty/deleted room IDs and
  flushes on a timer or when the batch reaches a configured size, so
  high-frequency state changes (e.g. signal buffering) collapse into a single
  file write per flush interval. Enabled by `STATE_DIR`; disabled by default.

- `loadtest/` — containerized load test harness (generator + runner). See
  `loadtest/README.md`; results live in `docs/LOAD_TEST_RESULTS.md`.

## Versioning

The repo-root `VERSION` file (a single `major.minor.patch` line) is the source
of truth for both the server and the client. `scripts/build.sh` stamps it into
the server binary via `-ldflags`; `scripts/release-client.sh` syncs it into
`package.json`, `client/package.json`, and `client/src/version.ts`.

**Any non-backward-compatible change to the protocol or the client API must
increment the major version number.** This includes: new required fields on
existing wire messages, removed or renamed fields, changed message semantics,
new required handshake fields, changed error/retry semantics, or breaking
changes to the client's public API. The server rejects clients whose major
version differs with `UNSUPPORTED_PROTOCOL_VERSION` (1402, not retryable) —
see `server/internal/server/server.go` `checkProtocolVersion` and
`client/src/peer-connection.ts` for the handshake `protocolVersion` field.

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
SHARD_NAME="t" \
LISTEN_ADDR=":8080" \
go run ./cmd/server
```

`SERVER_SECRET` (>=32 bytes), `ALLOWED_ORIGINS`, and `SHARD_NAME` are required;
the server refuses to start without them. `SHARD_NAME` is a single alphabetic
Crockford base32 character (`a-z` excluding `i`, `l`, `o`, `u`) baked into
every generated room id. Use `*` for `ALLOWED_ORIGINS` to disable origin
checking (logs a warning). See `docs/DESIGN.md` §"Configuration reference" for
the full list.

### Disk persistence (optional)

Set `STATE_DIR` to a directory to enable room state persistence. The server
writes one JSON file per room (atomic temp+rename) and rehydrates live rooms
on restart, so clients can rejoin after a server crash or rolling restart.
Writes are queued and batched by the `Persister` (see `internal/state`):
state changes mark a room "dirty" on a buffered channel, and a single flush
goroutine coalesces dirty/deleted room IDs and writes them on a timer
(`STATE_FLUSH_INTERVAL_MS`, default 100ms) or when the batch reaches
`STATE_BATCH_SIZE` (default 256). This bounds disk write frequency regardless
of how many times a room is dirtied. Disabled by default (empty `STATE_DIR`).

## Client (`client/`)

A zero-dependency TypeScript WebRTC client that speaks the protocol in
`docs/DESIGN.md` against the Go server above. It owns its WebRTC engine rather
than depending on `simple-peer` — see `docs/RTC_ENGINE_PLAN.md`. Full notes in
`client/AGENTS.md`; summary below.

### Layout

- `client/src/types.ts` — protocol message types, `CloseCode`, `ErrorCode`.
- `client/src/rtc/` — the WebRTC engine: `RtcPeer` (lifecycle), `negotiation`,
  `ice`, `channels` + `channel-handle` (multi-channel data), `media`, `signal`
  (peer-to-peer wire format), `env` (the one `RTCPeerConnection` seam).
- `client/src/peer-connection.ts` — `PeerConnection`, the public wrapper that
  ties `RtcPeer` to the protocol state machine.
- `client/src/transport.ts` — WebSocket wrapper (global `WebSocket`,
  browsers + Node >= 22).
- `client/src/storage.ts` — `sessionStorage` persistence of
  `{ roomId, role, rejoinToken, hostEpoch, guestEpoch }`.
- `client/src/errors.ts` — `SignalingError` mapping `error-response` and close
  codes to the retry policy.
- `client/src/util.ts` — base64url epoch generation, sequence counter,
  full-jitter backoff.
- `client/src/emitter.ts` — minimal typed event emitter (no `events` dep).
- `client/src/logger.ts` — the shared `Logger` interface.
- `client/src/roomid.ts` — frontend room id normalization (Crockford base32
  fuzzy decoding, no validation; backend owns rejection).
- `client/src/manifest.ts` — the client manifest: shard directory (weighted
  selection for hosts, room-id prefix lookup for guests), loaded from a URL or
  a static object. ICE/TURN config is not carried here; the server mints
  short-lived TURN credentials and returns them in the handshake responses.
  See README §"Shard manifest" and §"TURN credentials".
- `client/src/version.ts` — `PROTOCOL_VERSION` constant, stamped from the
  repo-root `VERSION` file by `scripts/release-client.sh`. Sent in every
  handshake message so the server can reject major-version mismatches.
- `client/src/index.ts` — public barrel.
- `client/test/` — `node:test` suite. `fakes.ts` provides a fake WebSocket and a
  fake peer for protocol tests; `rtc-fakes.ts` provides a fake
  `RTCPeerConnection` and `RTCDataChannel` for engine tests. No browser or
  native WebRTC needed.

#- `loadtest/` — containerized load test harness (generator + runner). See
  `loadtest/README.md`; results live in `docs/LOAD_TEST_RESULTS.md`.

## Build / test

```sh
cd client
npm install
npm install-scripts approve esbuild   # one-off: unblock tsx's postinstall
npm run build       # tsc -p tsconfig.build.json  -> dist/
npm run typecheck   # tsc -p tsconfig.json --noEmit (src + test)
npm run lint        # eslint .  (flat config, type-checked)
npm test            # node --test --import tsx test/*.test.ts (197 tests)
```

The client has **no runtime dependencies**. Keep it that way.

### Tooling notes

- TypeScript 5.9.3 (typescript-eslint 8.x does not yet support TS 7).
- ESLint 9.39.5 flat config (`eslint.config.js`); type-checked rules via
  `projectService`. `tsconfig.json` includes `src` + `test` (noEmit) for lint;
  `tsconfig.build.json` emits only `src`.
- Imports use the NodeNext `.js` extension convention.
- `tsconfig.build.json` sets `types: []`, so `src/` must not reference Node
  globals (`require`, `Buffer`, `process`).
- `ws` is an optional peer dependency (Node only); browsers use native
  `WebSocket`. RTC itself is browser-only for supported purposes.
- `npm audit` flags a high-severity advisory in `brace-expansion` (transitive
  via eslint's `minimatch`); the fix requires eslint 10.8.0, published
  2026-07-24, which is within the 7-day freshness window and therefore pinned
  to 9.39.5. It is a dev-only transitive DoS in eslint's glob handling and does
  not affect the shipped library.

### Protocol behaviors implemented

- `createRoom` (host) / `joinRoom` (guest) / `rejoin` (recovery from a
  persisted session). Every handshake message carries `protocolVersion` (from
  `client/src/version.ts`); the server rejects a major-version mismatch with
  `UNSUPPORTED_PROTOCOL_VERSION` (1402, not retryable). `joinRoom` normalizes
  the user-entered room id via Crockford base32 fuzzy decoding
  (case-insensitive, `O→0`, `I→1`, `L→1`) without rejecting malformed ids —
  the backend owns validation per `docs/ROOM_ID_SPEC.md` §"Frontend handling".
- Host waits for `guest-joined` before building the initiator peer; guest
  builds the non-initiator peer immediately on join.
- `signal` ↔ `signal-response` relay with per-(slot,epoch) `seq`.
- Sends `peer-connected` on `connect`; treats close `4200` (room-idle-close) as
  success — no reconnect.
- Renegotiation routed peer-to-peer over a dedicated control channel once the
  server has released the sockets, keeping protocol frames off the application's
  data stream.
- Reconnect with a fresh epoch + `rejoin-room` on retryable closes (`4008`,
  `4014`, `4300`); terminal handling for `4013` / `4400`.
- Symmetric epoch comparison: rebuilds the peer when the remote's epoch changed
  (`peer-reset`), keeps it on `peer-rejoined`; roles/initiator never change.
  Data channel handles survive the rebuild.
- `SIGNAL_BUFFER_OVERFLOW` → rebuild with new epoch; `INVALID_REJOIN_TOKEN` /
  `ROOM_CLOSED` / `ROOM_EXPIRED` / `UNSUPPORTED_PROTOCOL_VERSION` → terminal.
- Any number of application data channels with independent ordering and
  reliability, declared via `dataChannels` or opened at runtime. The initiator
  creates them and the responder binds by label, so the two sides cannot
  disagree about a channel's configuration.
- Shard selection from a client manifest (`manifest: {url}` fetched, or
  `{static}` in memory): hosts pick by weight before `create-room`, guests and
  rejoins pick by the room id's shard prefix, and the shard is pinned for the
  life of the connection. `url` remains supported as a one-shard shorthand.
- `PeerConnection` accepts injectable `transportFactory` and `peerFactory`
  options so the state machine is unit-testable without a browser or native
  WebRTC.
