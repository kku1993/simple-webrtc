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

The server requires a `SERVER_SECRET` (>=32 bytes), an `ALLOWED_ORIGINS`
allowlist, and a `SHARD_NAME` (a single alphabetic Crockford base32 character,
`a-z` excluding `i`, `l`, `o`, `u`); it refuses to start without them.

```sh
SERVER_SECRET="$(openssl rand -base64 32)" \
ALLOWED_ORIGINS="http://localhost:5173,https://your.app" \
SHARD_NAME="t" \
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
- `on()` and `once()` return an unsubscribe function, so you can release a
  listener without retaining a reference to the handler:
  ```ts
  const off = peer.on("stream", (s) => (video.srcObject = s));
  // later, e.g. in a React useEffect cleanup:
  off();
  ```

## Voice and video

The client ships first-class media support. `PeerConnection` exposes
`addTrack`, `removeTrack`, `replaceTrack`, `addStream`, `removeStream`, and a
higher-level `setLocalStream`. Tracks registered through these methods are
retained in a desired-media registry and re-attached to every internal
`SimplePeer` generation (initial construction and rebuilds after `peer-reset`),
so callers never need to know whether the underlying peer currently exists.

### Audio-only chat (after a button click)

```ts
import { PeerConnection, BrowserSessionStore } from "@simple-peer-signal/client";

const host = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  store: new BrowserSessionStore(),
});

// Acquire the microphone from a user gesture, then create the room. The
// track is attached to the SimplePeer when it is built (after guest-joined).
const enableMicBtn = document.getElementById("enable-mic")!;
enableMicBtn.addEventListener("click", async () => {
  const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  host.setLocalStream(stream);
  const { roomId } = await host.createRoom();
  console.log("share this room id:", roomId);
});

host.on("stream", (remoteStream) => {
  // Autoplay may be blocked; see "Browser media constraints" below.
  const audio = new Audio();
  audio.srcObject = remoteStream;
  audio.play().catch(() => {
    // Surface a "Play audio" button to the user.
  });
});
```

### Camera and microphone video chat

```ts
const guest = new PeerConnection({ url: "wss://signal.example/v1/signal" });

const joinBtn = document.getElementById("join")!;
joinBtn.addEventListener("click", async () => {
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: true,
    video: { width: 640, height: 360 },
  });
  // Attach the local preview.
  document.querySelector<HTMLVideoElement>("#local")!.srcObject = stream;
  guest.setLocalStream(stream);
  await guest.joinRoom({ roomId });
});

const remoteVideo = document.querySelector<HTMLVideoElement>("#remote")!;
guest.on("stream", (remoteStream) => {
  remoteVideo.srcObject = remoteStream;
});
```

### Muting without renegotiating

Mute a track by setting `track.enabled = false`. This does not require
renegotiation and works on any track the application owns:

```ts
const [mic] = host.currentLocalStream!.getAudioTracks();
mic.enabled = false; // muted; remote peer receives silence
```

### Switching microphones or cameras with `replaceTrack`

`replaceTrack` swaps a track on the active peer and updates the desired-media
registry, so the new track is re-attached after a peer rebuild. No manual
re-attach is needed on `peer-reset`.

```ts
const devices = await navigator.mediaDevices.enumerateDevices();
const newMic = devices.find((d) => d.kind === "audioinput" && d.label.includes("Headset"))!;
const newStream = await navigator.mediaDevices.getUserMedia({
  audio: { deviceId: { exact: newMic.deviceId } },
});
const [newTrack] = newStream.getAudioTracks();
const [oldTrack] = host.currentLocalStream!.getAudioTracks();
host.replaceTrack(oldTrack, newTrack, host.currentLocalStream!);
oldTrack.stop();
```

### Screen sharing, then returning to the camera

```ts
// Start screen share.
const display = await navigator.mediaDevices.getDisplayMedia({ video: true });
const [screenTrack] = display.getVideoTracks();
const [camTrack] = host.currentLocalStream!.getVideoTracks();
host.replaceTrack(camTrack, screenTrack, host.currentLocalStream!);

// Stop sharing and return to the camera.
screenTrack.addEventListener("ended", () => {
  host.replaceTrack(screenTrack, camTrack, host.currentLocalStream!);
});
```

### Cleaning up when leaving a room

The wrapper does not stop tracks it did not create. Stop your own tracks and
clear media elements on close:

```ts
host.on("close", () => {
  for (const t of host.currentLocalStream?.getTracks() ?? []) t.stop();
  remoteVideo.srcObject = null;
});
host.close();
```

### Host timing: requesting media while waiting for a guest

A host may request microphone access while waiting for a guest. Tracks
registered before `guest-joined` are attached to the `SimplePeer` when it is
built and included in the initial offer:

```ts
const host = new PeerConnection({ url: "wss://signal.example/v1/signal" });
const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
host.setLocalStream(stream); // safe to call before createRoom
await host.createRoom();
// ...later, guest-joined fires and the track is attached automatically.
```

You can also pass media at construction time via the `localStream` option,
which is convenient when media is acquired in a lobby screen before the
connection is created:

```ts
const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
const host = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  localStream: stream,
});
await host.createRoom();
```

### Initial negotiation vs renegotiation

- Tracks registered before the internal `SimplePeer` is constructed (host:
  before `guest-joined`; guest: before `joinRoom` resolves) are included in
  the **initial** offer/answer.
- Tracks added while signaling is in progress are attached to the existing
  peer via `addTrack`, which triggers `simple-peer`'s `signal` event and a
  renegotiation round.
- Multiple rapid `addTrack` calls are not batched by the wrapper; each one
  drives its own renegotiation. `simple-peer`/the browser may coalesce them.
- Negotiation glare (both peers add tracks simultaneously) is handled by
  `simple-peer`'s built-in renegotiation logic; the wrapper routes both
  directions over the data channel after socket release and forwards inbound
  `renegotiate` frames to `peer.signal()`.
- Media negotiation failures are reported via the `media-error` event
  (`{ message, cause? }`), distinct from the `error` event so applications
  can surface media failures without confusing them with room/signaling
  failures. The connection itself is unaffected by a media error.

### Reconnect guarantees for media

The wrapper retains desired tracks across reconnect scenarios:

1. **Signaling WebSocket reconnect where the WebRTC peer survives** — local
   tracks stay attached to the existing peer; no remote `stream`/`track`
   events re-fire.
2. **Remote peer rejoin with the same epoch** (`peer-rejoined`) — the
   WebRTC session continues; no media changes.
3. **Remote peer reset with a new epoch** (`peer-reset`) — the wrapper
   rebuilds its `SimplePeer` and re-attaches all live desired tracks to the
   new generation. Remote `stream`/`track` events fire again for the new
   peer; consumers should replace existing `srcObject` values.
4. **Full page reload** — browser media permission may persist, but the old
   `MediaStream` object cannot. Call `setLocalStream` again with a freshly
   acquired stream after `rejoin(session)`.

Use the `peer-created` and `peer-destroyed` events (with the monotonic
`peerGeneration` counter) to correlate media state across rebuilds.

### Browser media constraints

The signaling library cannot solve these browser responsibilities — the
application must handle them:

- `getUserMedia` requires a **secure context** except on `localhost`. Serve
  your app over HTTPS (or `http://localhost`).
- Microphone/camera permission should be requested from a **user gesture**
  (button click). Prompts fired on page load are often blocked.
- Remote audio/video **autoplay can be blocked**. Handle a rejected
  `HTMLMediaElement.play()` promise and surface a "Play audio" action.
- The application **owns `MediaStreamTrack.stop()`** unless documented
  otherwise. `PeerConnection.close()` clears retained references but does
  not stop caller-owned tracks.
- **Muting** with `track.enabled = false` does not require renegotiation.
- **Echo cancellation, noise suppression, and automatic gain control** are
  media constraints selected by the application, e.g.
  `getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } })`.

### TURN guidance

Data-channel tests on a local network can give a false impression that a
media application is production-ready. Voice/video users are far more likely
to encounter restrictive NATs and corporate networks. Provide STUN and TURN
servers through `simplePeer.config`:

```ts
const peer = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  simplePeer: {
    config: {
      iceServers: [
        { urls: "stun:stun.l.google.com:19302" },
        {
          urls: "turn:turn.example.org:3478",
          username: "user",
          credential: "pass",
        },
      ],
    },
  },
});
```

For real deployments, use **authenticated TURN** (rotating credentials via
`credentialType: "otp"` when your TURN server supports it). Verify relay
candidates by inspecting `RTCStatsReport` from `peer.getStats()` — look for
`candidateType: "relay"` in the ICE candidate pair reports. Avoid logging
SDP, candidate IPs, or device labels from stats by default.

### Diagnostics

`peer.mediaDiagnostics` returns a snapshot that avoids sensitive details:

```ts
{
  peerGeneration: 2,        // current internal SimplePeer generation
  hasPeer: true,
  peerConnected: true,
  desiredTrackCount: 2,     // tracks retained by the wrapper
  desiredTracks: [
    { kind: "audio", readyState: "live", id: "..." },
    { kind: "video", readyState: "live", id: "..." },
  ],
  hasLocalStream: true,
}
```

For deeper inspection, `await peer.getStats()` passes through to the
underlying `RTCPeerConnection.getStats()` (or returns `null` when no peer
exists). For advanced transceiver/sender control, use the `rawPeer` escape
hatch (typed as `SimplePeer.Instance | null`); prefer the wrapper-level
media methods for ordinary voice/video.

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
  SHARD_NAME="t" \
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
