# simple-peer-signal

Backend server (golang) and corresponding client library (typescript)
to handle signaling for [simple-peer](https://github.com/feross/simple-peer).

See `docs/DESIGN.md` for the full protocol, state machine, and configuration
reference. This README covers getting both sides running end-to-end.

## Quickstart

### 1. Run the signaling server

The server requires a `SERVER_SECRET` (>=32 bytes) and an `ALLOWED_ORIGINS`
allowlist; it refuses to start without them.

```sh
cd server
go run ./cmd/server \
  SERVER_SECRET="$(openssl rand -base64 32)" \
  ALLOWED_ORIGINS="http://localhost:5173,https://your.app" \
  LISTEN_ADDR=":8080"
```

Health and Prometheus metrics are served alongside the WebSocket endpoint:

- `GET /healthz` — liveness (`{ status, uptimeSec }`)
- `GET /metrics` — Prometheus text format
- `GET /v1/signal` — the WebSocket endpoint clients connect to

Use `ALLOWED_ORIGINS="*"` to disable origin checking (the server logs a
warning; do not do this in production). See `docs/DESIGN.md`
§"Configuration reference" for the full env var list (rate limits, room TTL,
peer deadline, Cloudflare Turnstile, etc.).

### 2. Build the client

```sh
cd client
npm install
npm install-scripts approve esbuild   # one-off: unblock tsx's postinstall
npm run build                         # emits ESM + .d.ts to dist/
```

`npm test` runs the state-machine suite against a fake WebSocket + fake
`simple-peer`, so no browser or `wrtc` is needed.

### 3. Use the client

The client ships as the `@simple-peer-signal/client` package; one
`PeerConnection` represents one room pairing. The host creates a room, the
guest joins it, and the client handles signaling, reconnection, and
renegotiation automatically.

#### Installing from a GitHub release

The client is distributed as a tarball attached to GitHub releases (tagged
after `server/VERSION`, e.g. `v0.1`). Install it in your project with:

```sh
npm install https://github.com/cognition/simple-peer-signal-server/releases/download/v0.1/simple-peer-signal-client-0.1.0.tgz
```

See the [releases page](https://github.com/cognition/simple-peer-signal-server/releases)
for available versions. To cut a new release, run:

```sh
scripts/release-client.sh             # builds, packs, creates the GitHub release
scripts/release-client.sh --dry-run   # build + pack only, no release
```

The script reads the version from `server/VERSION` and requires the `gh` CLI.

```ts
import { PeerConnection, BrowserSessionStore } from "@simple-peer-signal/client";

// Host side
const host = new PeerConnection({
  url: "ws://localhost:8080/v1/signal",
  store: new BrowserSessionStore(),   // persists rejoin state across reloads
});
host.on("guest-joined", () => console.log("guest is here, dialing..."));
host.on("connect", () => host.simplePeer?.send("hello from host"));
host.on("data", (d) => console.log("host got", d));
const { roomId } = await host.createRoom();
console.log("share this room id:", roomId);

// Guest side (in another tab / browser)
const guest = new PeerConnection({ url: "ws://localhost:8080/v1/signal" });
guest.on("connect", () => guest.simplePeer?.send("hello from guest"));
guest.on("data", (d) => console.log("guest got", d));
await guest.joinRoom({ roomId });

// Tear down
// host.close(); guest.close();
```

Notes:

- The host's `SimplePeer` is built only after `guest-joined` fires; the
  guest's is built immediately on `joinRoom`. Don't construct `SimplePeer`
  yourself — pass options via `PeerConnectionOptions.simplePeer`.
- After `connect`, the server releases both sockets (close `4200`) and
  renegotiation flows over the data channel as
  `{ kind: "renegotiate", signal }`. The client handles this for you.
- On a retryable socket close the client reconnects with `rejoin-room` and a
  fresh epoch, rebuilding the `SimplePeer` only when the peer's epoch changed.
  Persisted `BrowserSessionStore` state lets `peer.rejoin(session)` resume
  after a page reload.
- For Node usage, install the optional `ws` peer dependency; browsers use the
  native `WebSocket`.

## Repository layout

- `server/` — Go signaling server (`cmd/server` entry point, `internal/*` packages).
- `client/` — TypeScript source for `@simple-peer-signal/client` (wrapping `simple-peer`).
- `package.json` — root package face for `@simple-peer-signal/client` (installed via GitHub release tarballs).
- `scripts/` — `build.sh` (Go server binary), `release-client.sh` (client release tarball).
- `docs/DESIGN.md` — protocol, state machine, and configuration reference.
