# simple-peer-signal

Backend server (golang) and corresponding client library (typescript)
to handle signaling for [simple-peer](https://github.com/feross/simple-peer).

See `docs/DESIGN.md` for the full protocol, state machine, and configuration
reference. This README covers getting both sides running end-to-end.

## Quickstart

Both the server binary and the client library are distributed as artifacts
attached to [GitHub releases](https://github.com/kku1993/simple-peer-signal/releases),
tagged after the repo-root `VERSION` file (e.g. `v0.1.0`).

### 1. Run the signaling server

Download the prebuilt binary for your architecture from the latest release
(`x86_64` for linux/amd64, `arm64` for linux/arm64):

```sh
VERSION=0.1.0
ARCH=x86_64   # or arm64
curl -L -o simple-peer-signal-server \
  https://github.com/kku1993/simple-peer-signal/releases/download/v${VERSION}/simple-peer-signal-server-${VERSION}-${ARCH}
chmod +x simple-peer-signal-server
```

The server requires a `SERVER_SECRET` (>=32 bytes) and an `ALLOWED_ORIGINS`
allowlist; it refuses to start without them.

```sh
SERVER_SECRET="$(openssl rand -base64 32)" \
ALLOWED_ORIGINS="http://localhost:5173,https://your.app" \
LISTEN_ADDR=":8080" \
./simple-peer-signal-server
```

Health and Prometheus metrics are served alongside the WebSocket endpoint:

- `GET /healthz` — liveness (`{ status, uptimeSec }`)
- `GET /metrics` — Prometheus text format
- `GET /v1/signal` — the WebSocket endpoint clients connect to

Use `ALLOWED_ORIGINS="*"` to disable origin checking (the server logs a
warning; do not do this in production). See `docs/DESIGN.md`
§"Configuration reference" for the full env var list (rate limits, room TTL,
peer deadline, Cloudflare Turnstile, etc.).

### 2. Set up the client

The client ships as the `@simple-peer-signal/client` npm package, distributed
as a tarball attached to the same GitHub release. Install it in your project:

```sh
npm install https://github.com/kku1993/simple-peer-signal/releases/download/v0.1.0/simple-peer-signal-client-0.1.0.tgz
```

One `PeerConnection` represents one room pairing. The host creates a room,
the guest joins it, and the client handles signaling, reconnection, and
renegotiation automatically.

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
- The client uses the global `WebSocket` (browsers and Node >= 22); no
  `ws` dependency is required.

## Repository layout

- `server/` — Go signaling server (`cmd/server` entry point, `internal/*` packages).
- `client/` — TypeScript source for `@simple-peer-signal/client` (wrapping `simple-peer`).
- `package.json` — root package face for `@simple-peer-signal/client` (installed via GitHub release tarballs).
- `scripts/` — `build.sh` (Go server binary), `release-client.sh` (client release tarball).
- `docs/DESIGN.md` — protocol, state machine, and configuration reference.

## Building from source

Prebuilt artifacts are published on the
[releases page](https://github.com/kku1993/simple-peer-signal/releases).
The instructions below are only needed for local development or cutting a new
release.

### Server (Go)

```sh
cd server
go run ./cmd/server \
  SERVER_SECRET="$(openssl rand -base64 32)" \
  ALLOWED_ORIGINS="http://localhost:5173,https://your.app" \
  LISTEN_ADDR=":8080"
```

To build a static binary for a specific architecture (version read from the
repo-root `VERSION` file):

```sh
scripts/build.sh              # current host arch
scripts/build.sh x86_64       # linux/amd64
scripts/build.sh arm64        # linux/arm64
```

### Client (TypeScript)

```sh
cd client
npm install
npm install-scripts approve esbuild   # one-off: unblock tsx's postinstall
npm run build                         # emits ESM + .d.ts to dist/
```

`npm test` runs the state-machine suite against a fake WebSocket + fake
`simple-peer`, so no browser or `wrtc` is needed.

To build a release tarball (version read from the repo-root `VERSION` file,
written to `dist/simple-peer-signal-client-<version>.tgz`):

```sh
scripts/release-client.sh             # build + pack to dist/
```

Attach the resulting tarball to a GitHub release manually.
