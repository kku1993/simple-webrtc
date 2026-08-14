# simple-webrtc

Backend server (golang) and corresponding client library (typescript) for
peer-to-peer WebRTC: media, plus any number of ordered and unordered data
channels on a single connection.

The client ships its own WebRTC engine and has **no runtime dependencies**.

See `docs/DESIGN.md` for the full protocol, state machine, and configuration
reference. This README covers getting both sides running end-to-end.

## Quickstart

Both the server binary and the client library are distributed as artifacts
attached to [GitHub releases](https://github.com/kku1993/simple-webrtc/releases),
tagged after the repo-root `VERSION` file (e.g. `v0.8.0`).

### 1. Run the signaling server

Download the prebuilt binary for your architecture from the latest release
(`x86_64` for linux/amd64, `arm64` for linux/arm64):

```sh
VERSION=0.8.0
ARCH=x86_64   # or arm64
curl -L -o simple-webrtc-server \
  https://github.com/kku1993/simple-webrtc/releases/download/v${VERSION}/simple-webrtc-server-${VERSION}-${ARCH}
chmod +x simple-webrtc-server
```

The server requires a `SERVER_SECRET` (>=32 bytes), an `ALLOWED_ORIGINS`
allowlist, and a `SHARD_NAME` (a single alphabetic Crockford base32 character,
`a-z` excluding `i`, `l`, `o`, `u`); it refuses to start without them.

Each server process is one **shard**. Rooms live in that process's memory, and
`SHARD_NAME` becomes the first character of every room id it mints, so a room id
names the process that owns it. Production runs several shards and points
clients at them with a [manifest](#shard-manifest); a single shard is enough for
local development.

```sh
SERVER_SECRET="$(openssl rand -base64 32)" \
ALLOWED_ORIGINS="http://localhost:5173,https://your.app" \
SHARD_NAME="t" \
LISTEN_ADDR=":8080" \
./simple-webrtc-server
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

The client ships as the `@simple-webrtc/client` npm package, distributed
as a tarball attached to the same GitHub release. Install it in your project:

```sh
npm install https://github.com/kku1993/simple-webrtc/releases/download/v0.8.0/simple-webrtc-client-0.8.0.tgz
```

One `PeerConnection` represents one room pairing. The host creates a room,
the guest joins it, and the client handles signaling, reconnection, and
renegotiation automatically.

```ts
import { PeerConnection, BrowserSessionStore } from "@simple-webrtc/client";

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

`joinRoom` normalizes the user-entered room id for you (Crockford base32
fuzzy decoding: case-insensitive, `O→0`, `I→1`, `L→1`) before sending it to
the server, so callers do not need to preprocess the input. If you want to
display the canonical form to the user — e.g. to render `ta0000` instead of
the `TA OOOO` they typed — use the exported `normalizeRoomId` helper, which
applies the same transform without validating or rejecting:

```ts
import { normalizeRoomId } from "@simple-webrtc/client";

normalizeRoomId("TA OOOO"); // "ta 0000"
normalizeRoomId("Il1");     // "1111"
```

The helper never throws; the server remains the single source of truth for
validation. See `docs/ROOM_ID_SPEC.md` §"Frontend handling".

Notes:

- `url` is the single-endpoint shorthand, good for a local server. Production
  deployments pass `manifest` instead, so clients discover shards at runtime —
  see [Shard manifest](#shard-manifest). Exactly one of the two is required.
  TURN credentials come from the server's handshake response, not the manifest
  — see [TURN credentials](#turn-credentials).
- The host's peer is built only after `guest-joined` fires; the guest's is
  built immediately on `joinRoom`. Don't construct one yourself — the server
  supplies ICE configuration in the handshake response, and an app can
  override it via `PeerConnectionOptions.rtc`.
- After `connect`, the server releases both sockets (close `4200`) and
  renegotiation flows peer-to-peer over a dedicated control channel, separate
  from application data. The client handles this for you.
- On a retryable socket close the client reconnects with `rejoin-room` and a
  fresh epoch, rebuilding the peer only when the peer's epoch changed.
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

## Shard manifest

The signaling backend is a set of single-process shards. A room lives entirely
in one process's memory, and the shard's `SHARD_NAME` is the first character of
every room id it mints — so a room id names the process that owns it.

Clients find those processes through a **manifest**: a JSON config file served
from a URL you control.

- The **host always loads the manifest before creating a room** and picks a
  shard by weighted random choice. Load balancing is therefore a config change,
  not a deploy.
- The **guest loads the same manifest and picks the shard that owns the room id**
  it was given, so it lands on the process holding the room.
- The manifest is the client's shard directory. ICE/TURN configuration is not
  carried here: the server mints short-lived TURN credentials per connection
  and returns them in the handshake responses, so credentials rotate without a
  manifest republish — see [TURN credentials](#turn-credentials).

### Manifest format

```json
{
  "version": 1,
  "shards": [
    { "name": "t", "url": "wss://t.signal.example/v1/signal", "weight": 3 },
    { "name": "k", "url": "wss://k.signal.example/v1/signal", "weight": 1 },
    { "name": "m", "url": "wss://m.signal.example/v1/signal", "weight": 0 }
  ],
  "settings": { "anything": "your app wants" }
}
```

| Field | Meaning |
| --- | --- |
| `version` | Schema version. Currently `1`; a client refuses a manifest newer than it understands. Optional, defaults to `1`. |
| `shards[].name` | The shard's `SHARD_NAME` — the room-id prefix it owns. `"*"` is a wildcard that matches every room id. |
| `shards[].url` | The shard's WebSocket endpoint. Must be `ws://` or `wss://`. |
| `shards[].weight` | Relative share of **new** rooms. Optional, defaults to `1`. `0` drains the shard. |
| `settings` | Opaque application config. The client never reads it; get it from `peer.currentManifest?.settings`. |

A copy of this example lives at `docs/signal-manifest.example.json`.

Room ids are normalized (Crockford base32 fuzzy decoding) before the shard
lookup, so a user typing `T1A230` reaches the same shard as `t1a230`. The
shard whose `name` is the **longest** matching prefix wins, and a `"*"` entry is
consulted only when no named shard matches. `joinRoom` applies this transform
internally; the exported `normalizeRoomId` helper (see
[Quickstart](#2-set-up-the-client)) exposes the same logic for display or
pre-routing purposes.

### Pointing clients at it

```ts
const peer = new PeerConnection({
  manifest: { url: "https://config.example.com/signal-manifest.json" },
});
```

Serve the file from any static host or CDN. Two requirements:

- **CORS**: the manifest is fetched with `fetch()` from your app's origin, so
  the response needs `Access-Control-Allow-Origin` for that origin.
- **Cache headers**: pick a `max-age` you are willing to wait when draining a
  shard. The client also caches in memory for `ttlMs` (default 60s).

Full options:

```ts
const peer = new PeerConnection({
  manifest: {
    url: "https://config.example.com/signal-manifest.json",
    ttlMs: 60_000,       // reuse a fetched manifest this long (default 60s)
    timeoutMs: 5_000,    // per-request timeout (default 5s)
    fallback: LOCAL_MANIFEST, // used if the very first fetch fails
    // fetch: customFetch,    // inject your own fetch (tests, auth headers)
  },
});
```

If a **refresh** fails, the client keeps using the last manifest it loaded —
losing the config server does not take down clients that already know where the
shards are. If the **first** load fails, the `fallback` is used when configured;
otherwise `createRoom()` / `joinRoom()` reject with a `ManifestError` before any
socket is opened.

### Static config for local testing

Pass the manifest in memory instead of fetching it. This is the recommended
shape for local development and tests — same code path, no config server:

```ts
import { PeerConnection, type SignalManifest } from "@simple-webrtc/client";

const LOCAL_MANIFEST: SignalManifest = {
  version: 1,
  shards: [{ name: "t", url: "ws://localhost:8080/v1/signal" }],
};

const peer = new PeerConnection({ manifest: { static: LOCAL_MANIFEST } });
```

A static manifest is validated eagerly, so a malformed one throws from the
`PeerConnection` constructor rather than at connect time. Selecting the shard
for a static manifest involves no I/O, so `createRoom()` opens its socket in the
same tick — identical to passing `url` directly, which is just sugar for a
one-wildcard-shard manifest.

### Operating it

- **Add a shard**: start the process with a fresh `SHARD_NAME`, then add its
  entry to the manifest. New rooms start arriving as clients pick up the change.
- **Drain a shard**: set its `weight` to `0`. No new rooms are placed there, but
  guests holding an existing room id are still routed to it, so live rooms
  finish normally. Take the process down after your room TTL has elapsed.
- **Reweight**: weights are relative, not percentages — `3` and `1` send 75% /
  25% of new rooms.
- **Never reuse a `SHARD_NAME` for a different endpoint** while rooms may still
  exist on the old one; the room id is the only routing key.
- A connection **pins its shard for its lifetime**. Reconnects and
  `rejoin(session)` return to the same shard, because that is where the room is
  — a manifest edit never moves a live room.

### TURN credentials

The server builds the `iceServers` array for every handshake response
(`create-room-response`, `join-room-response`, `rejoin-room-response`):
**Google's public STUN server is always included**, and when TURN is configured,
short-lived Cloudflare TURN credentials are appended. The client applies them to
each peer generation, including peers rebuilt after a reconnect, so a `rejoin`
rotates credentials without an app deploy.

#### How the backend assigns `iceServers`

The backend is the default source of ICE configuration. On every handshake the
server returns an `iceServers` array in the response body; the client captures
it and feeds it to the `RTCPeerConnection` it builds for that connection (and
to every rebuilt peer after a `peer-reset` or `rejoin`). No client-side
configuration is required for the server-provided set to take effect — this is
the path to use when you want the backend to own STUN/TURN selection and
credential rotation:

```ts
// The server supplies iceServers; nothing to configure on the client.
const peer = new PeerConnection({
  url: "wss://signal.example/v1/signal",
});
```

A server that omits the `iceServers` field on a later response (e.g. a
`rejoin-room-response`) does not strip the previously captured set, so a
connection that already has TURN keeps it for the rest of its life.

#### Frontend config overrides the backend

An application that wants to control its own STUN/TURN set can override the
server-provided `iceServers` by passing `rtc.config.iceServers` to
`PeerConnection`. **Locally supplied values win outright**: when
`rtc.config.iceServers` is set, the server-provided array is not merged in —
the application's array replaces it entirely. Other fields on `rtc.config`
(`iceTransportPolicy`, `bundlePolicy`, etc.) are still applied alongside
either ICE source.

```ts
const peer = new PeerConnection({
  manifest: { url: "https://config.example.com/signal-manifest.json" },
  // Overrides the server-provided iceServers — the backend's array is NOT
  // merged in; this list is used verbatim for every peer generation.
  rtc: { config: { iceServers: [{ urls: "stun:stun.l.google.com:19302" }] } },
});
```

The precedence, from the client's perspective:

1. **`rtc.config.iceServers` set by the app** → used verbatim; the server's
   `iceServers` is ignored.
2. **`rtc.config.iceServers` unset, server returns `iceServers`** → the
   server's array is applied (merged with any other `rtc.config` fields).
3. **Neither side supplies ICE config** → the peer is built with no `config`
   override, so the browser's defaults apply.

This means an operator can deploy the backend with TURN enabled and have every
client pick it up automatically, while still leaving the door open for an
application that needs to host its own STUN/TURN infrastructure to take over
with a single `rtc.config.iceServers` override — no server change required.

When the server is not configured to mint TURN credentials (the default), the
`iceServers` field still carries Google's STUN server, so clients always have a
STUN path. See [TURN guidance](#turn-guidance) for choosing and verifying TURN
servers.

#### Server-side setup

The server uses the [Cloudflare Calls TURN API](https://developers.cloudflare.com/realtime/turn/generate-credentials/)
to mint credentials. Create a TURN key in the Cloudflare dashboard, then set
these environment variables on the signaling server:

| Variable | Meaning |
| --- | --- |
| `TURN_KEY_ID` | The TURN key id (shown in the dashboard). |
| `TURN_KEY_API_TOKEN` | The TURN key's API token (shown once at creation). |
| `TURN_CREDENTIAL_TTL_SEC` | Credential lifetime in seconds. Optional, defaults to `14400` (4 hours). |

Both `TURN_KEY_ID` and `TURN_KEY_API_TOKEN` must be set together; the server
refuses to start if only one is present. When both are unset (the default),
TURN is disabled and the `iceServers` field carries only Google's STUN server.
The key material stays server-side — only the short-lived username/credential
pair is sent to clients. Each credential is tagged with `roomId-unixTimestamp`
in Cloudflare's analytics so usage can be aggregated per session even when room
ids are recycled.

### Inspecting the resolution

```ts
peer.currentShard;     // { name: "k", url: "wss://k.signal.example/v1/signal", weight: 1 } | null
peer.signalUrl;        // "wss://k.signal.example/v1/signal" | null
peer.currentManifest;  // the SignalManifest in force, incl. `settings` | null
```

For tests, `shardRandom` replaces `Math.random` in weighted selection:

```ts
new PeerConnection({ manifest: { static: M }, shardRandom: () => 0 }); // always the first shard
```

The pieces are exported standalone too — `parseManifest`, `ManifestProvider`,
`selectShard`, `shardForRoomId`, `singleShardManifest`, and `ManifestError` —
if you want to resolve or validate a manifest yourself.

## Data channels

A connection carries any number of application data channels, each with its own
ordering and reliability. Declare them on both sides:

```ts
const peer = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  dataChannels: {
    chat: {},                                       // ordered + reliable (default)
    cursor: { ordered: false, maxRetransmits: 0 },  // unordered, fire-and-forget
    telemetry: { ordered: false, maxPacketLifeTime: 100 }, // partially reliable
  },
});

const chat = peer.channel("chat");
chat.on("message", (data) => render(data));
chat.send("hello");

peer.channel("cursor").send(`${x},${y}`);
```

Notes:

- `peer.channel(label)` works immediately — before `createRoom`/`joinRoom` and
  before any peer exists. The handle reports `readyState: "connecting"` until
  the channel opens.
- **Handle identity is stable, channel state is not.** The returned object
  survives peer rebuilds, so you can attach listeners once and keep the
  reference for the life of the connection. Gate sends on `readyState`, or let
  the `whenClosed` policy do it.
- `whenClosed` defaults to `"buffer"` for ordered reliable channels and
  `"throw"` for everything else, because flushing stale cursor positions after
  a reconnect is as wrong as dropping the first chat message. Override per
  channel with `"buffer"`, `"throw"`, or `"drop"`.
- The host creates every declared channel and the guest binds by label, so a
  channel's ordering and reliability always come from the host — the two sides
  cannot disagree about them. A label declared only by the guest never opens
  and is logged.
- `peer.openChannel(label, spec)` opens one at runtime from either side; the
  remote learns about it through the `channel` event.
- `peer.on("data", ...)` and `peer.send(...)` still work and use a built-in
  default channel. Labels beginning with `__sps_` are reserved.
- Backpressure: read `bufferedAmount` and listen for `drain`. The library does
  not chunk — SCTP caps a message at roughly 256 KiB and 64 KiB is the safe
  cross-browser figure, so frame large payloads yourself.

## Voice and video

The client ships first-class media support. `PeerConnection` exposes
`addTrack`, `removeTrack`, `replaceTrack`, `addStream`, `removeStream`, and a
higher-level `setLocalStream`. Tracks registered through these methods are
retained in a desired-media registry and re-attached to every internal peer
generation (initial construction and rebuilds after `peer-reset`), so callers
never need to know whether the underlying peer currently exists.

### Audio-only chat (after a button click)

```ts
import { PeerConnection, BrowserSessionStore } from "@simple-webrtc/client";

const host = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  store: new BrowserSessionStore(),
});

// Acquire the microphone from a user gesture, then create the room. The
// track is attached to the peer when it is built (after guest-joined).
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
registered before `guest-joined` are attached to the peer when it is built
and included in the initial offer:

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

- Tracks registered before the internal peer is constructed (host: before
  `guest-joined`; guest: before `joinRoom` resolves) are included in the
  **initial** offer/answer.
- Tracks added while signaling is in progress are attached to the existing
  peer via `addTrack`, which drives a renegotiation round.
- Multiple `addTrack` calls made in the same tick are batched into a single
  renegotiation, and a request arriving mid-exchange is queued and runs once
  when signaling returns to `stable`.
- Negotiation glare cannot occur: the host is permanently the initiator and is
  the only side that ever creates an offer. The guest requests renegotiation by
  signaling instead, so the two sides can never offer simultaneously.
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
   rebuilds its peer and re-attaches all live desired tracks to the
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
to encounter restrictive NATs and corporate networks. The recommended path is
to configure the signaling server to mint short-lived TURN credentials via the
Cloudflare Calls TURN API — see [TURN credentials](#turn-credentials). The
server then returns an `iceServers` array in every handshake response and the
client applies it automatically, so no client-side configuration is needed:

```ts
const peer = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  // No rtc.config needed — the server supplies iceServers.
});
```

To supply your own STUN/TURN servers instead (or in addition), override via
`rtc.config.iceServers`; locally supplied values win over the server-provided
set:

```ts
const peer = new PeerConnection({
  url: "wss://signal.example/v1/signal",
  rtc: {
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

For real deployments, use **authenticated TURN** with rotating credentials —
the server-provided path does this automatically. Verify relay candidates by
inspecting `RTCStatsReport` from `peer.getStats()` — look for
`candidateType: "relay"` in the ICE candidate pair reports. Avoid logging
SDP, candidate IPs, or device labels from stats by default.

### Diagnostics

`peer.mediaDiagnostics` returns a snapshot that avoids sensitive details:

```ts
{
  peerGeneration: 2,        // current internal peer generation
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

`peer.dataChannelDiagnostics` returns the same kind of snapshot per data
channel (`label`, `readyState`, `ordered`, `bufferedAmount`, `queued`, `id`),
never message contents.

For deeper inspection, `await peer.getStats()` passes through to the underlying
`RTCPeerConnection.getStats()` (or returns `null` when no peer exists) and
returns the native `RTCStatsReport` unmodified. For advanced transceiver/sender
control, use the `peerInstance` escape hatch and its `peerConnection` getter;
prefer the wrapper-level media and channel methods for ordinary use.

## Repository layout

- `server/` — Go signaling server (`cmd/server` entry point, `internal/*` packages).
- `client/` — TypeScript source for `@simple-webrtc/client` (includes `src/rtc/`, the WebRTC engine).
- `package.json` — root package face for `@simple-webrtc/client` (installed via GitHub release tarballs).
- `scripts/` — `build.sh` (Go server binary), `release-client.sh` (client release tarball).
- `docs/DESIGN.md` — protocol, state machine, and configuration reference.
- `docs/signal-manifest.example.json` — example client manifest (shard directory).

## Building from source

Prebuilt artifacts are published on the
[releases page](https://github.com/kku1993/simple-webrtc/releases).
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

`npm test` runs the suite against a fake WebSocket, a fake peer, and a fake
`RTCPeerConnection`, so no browser or native WebRTC is needed.

To build a release tarball (version read from the repo-root `VERSION` file,
written to `dist/simple-webrtc-client-<version>.tgz`):

```sh
scripts/release-client.sh             # build + pack to dist/
```

Attach the resulting tarball to a GitHub release manually.
