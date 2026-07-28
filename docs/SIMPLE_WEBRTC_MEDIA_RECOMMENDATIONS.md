# Recommendations for `simple-webrtc` media support

These recommendations come from adding optional P2P voice chat to This Game Has
Legs using `@simple-webrtc/client` v0.1.0. The existing library already does
two important things correctly:

- It forwards `simple-peer`'s `stream` and `track` events as typed
  `PeerConnection` events.
- It carries renegotiation signaling over the data channel after the signaling
  WebSocket is released.

That made voice possible without changing the signaling server. The main
friction was managing local media across the wrapper's delayed peer creation
and peer-rebuild lifecycle.

## Highest-priority recommendations

### 1. Expose media methods on `PeerConnection`

Applications currently have to reach through `peer.simplePeer`, cast the
restricted `PeerLike` type, check whether the underlying peer exists, and call
`addTrack` themselves. This leaks an implementation detail and is especially
awkward for a host because its `SimplePeer` is not constructed until after the
guest joins.

Add first-class methods such as:

```ts
interface PeerConnection {
  addTrack(track: MediaStreamTrack, stream: MediaStream): void;
  removeTrack(track: MediaStreamTrack, stream: MediaStream): void;
  replaceTrack(
    oldTrack: MediaStreamTrack,
    newTrack: MediaStreamTrack,
    stream: MediaStream,
  ): Promise<void> | void;
  addStream(stream: MediaStream): void;
  removeStream(stream: MediaStream): void;
}
```

The wrapper should accept these calls before room creation, while waiting for a
peer, during signaling, and after connection. Callers should not need to know
whether `simplePeer` is currently non-null.

### 2. Retain local media across peer creation and rebuilds

`PeerConnection` can destroy and recreate its internal `SimplePeer` when an
epoch changes. Any tracks added directly to the old instance are then lost.
Applications must currently keep their own track registry and reattach every
track to every newly created peer.

The library should own a desired-media registry:

1. `addTrack` records the track and stream even when no internal peer exists.
2. When a `SimplePeer` is constructed, all desired tracks are attached once.
3. When the internal peer is rebuilt, live desired tracks are attached to the
   replacement.
4. `removeTrack` removes the entry so it is not restored later.
5. Ended tracks are removed or ignored.

Deduplication must be based on the pair of internal peer instance and track.
`simple-peer.addTrack()` returns `void`, so its return value cannot indicate
whether attachment succeeded.

This would remove most of the adapter code that was needed in this project.

### 3. Offer a stream-oriented convenience API

Most voice/video applications think in terms of one local stream rather than
individual RTP senders. A higher-level API would make the common case much
simpler:

```ts
const peer = new PeerConnection({ url, store });

peer.setLocalStream(stream);
peer.on("stream", (remoteStream) => {
  remoteVideo.srcObject = remoteStream;
});
```

Useful semantics for `setLocalStream` would be:

- It may be called before or after `createRoom`/`joinRoom`.
- Replacing the stream adds, removes, or replaces tracks as required.
- Passing `null` stops sending without requiring the library to stop tracks
  owned by the application.
- The selected stream survives internal peer reconstruction.

An optional constructor field such as `localStream` could support applications
that acquire media before connecting, while `setLocalStream` supports the more
common browser flow where access is requested after a user gesture.

### 4. Expose an explicit internal-peer lifecycle event

Even with first-class media methods, advanced consumers may need access to the
underlying `simple-peer` instance for transceivers, sender parameters, screen
sharing, or statistics. Add an event that clearly identifies each internal
peer generation:

```ts
peer.on("peer-created", ({ peer: simplePeer, generation }) => {});
peer.on("peer-destroyed", ({ generation, reason }) => {});
```

This is safer than asking consumers to infer peer construction from
`guest-joined`, `status`, or `connect`:

- On the host, `guest-joined` is emitted before the internal peer is built.
- `status: "signaling"` can also be emitted during construction.
- `connect` is late for applications that want tracks included in the initial
  offer.

The event should document whether media registered through the wrapper has
already been attached when it fires.

## API and lifecycle clarity

### 5. Document initial negotiation versus renegotiation

The README explains that renegotiation travels over the data channel after
socket release, but a media example should state:

- Whether tracks registered before peer construction are included in the
  initial offer.
- What happens when tracks are added while signaling is in progress.
- Whether multiple rapid `addTrack` calls are batched.
- How negotiation glare is handled if both peers add tracks simultaneously.
- What errors or timeouts are emitted when renegotiation fails.

A typed event such as `negotiation-state` or `media-error` would help
applications distinguish media negotiation failures from room/signaling
failures.

### 6. Clarify reconnect guarantees for media

Document the behavior separately for:

1. A signaling WebSocket reconnect where the current WebRTC peer survives.
2. A remote peer rejoin with the same epoch.
3. A remote peer reset with a new epoch and a rebuilt `SimplePeer`.
4. A full page reload, where browser media permission may persist but the old
   `MediaStream` object cannot.

In particular, state explicitly whether local tracks are retained, whether
remote `stream`/`track` events fire again, and whether consumers must replace
existing `srcObject` values.

### 7. Return unsubscribe functions from event registration

The current emitter requires callers to retain handlers and call `off`
explicitly. UI integrations are simpler and less error-prone when `on` returns
an unsubscribe callback or when a dedicated method is available:

```ts
const unsubscribe = peer.subscribe("stream", handleStream);
unsubscribe();
```

This is valuable for React and other component frameworks, particularly when a
connection survives navigation between lobby and game screens.

## Documentation additions

### 8. Add complete voice and video examples

The client README currently demonstrates data messages only. Add small,
copyable browser examples for:

- Audio-only chat enabled after a button click.
- Camera and microphone video chat.
- Muting by setting `track.enabled` without renegotiating.
- Switching microphones or cameras with `replaceTrack`.
- Screen sharing followed by returning to the camera.
- Cleaning up local tracks and media elements when leaving a room.

The examples should cover both host and guest timing. In particular, show that
a host may request microphone access while waiting for a guest and that the
track will be attached when the peer is created.

### 9. Document browser media constraints

A media guide should call out browser responsibilities that the signaling
library cannot solve:

- `getUserMedia` requires a secure context except on localhost.
- Microphone/camera permission should be requested from a user gesture.
- Remote audio/video autoplay can be blocked; applications must handle a
  rejected `HTMLMediaElement.play()` promise and may need a “Play audio” action.
- The application owns `MediaStreamTrack.stop()` unless the API explicitly says
  otherwise.
- Muting with `track.enabled = false` does not require renegotiation.
- Echo cancellation, noise suppression, and automatic gain control are media
  constraints selected by the application.

### 10. Include TURN guidance in the media quickstart

Data-channel tests on a local network can give a false impression that a media
application is production-ready. The docs should explain how to provide STUN
and TURN servers through `simplePeer.config`, recommend authenticated TURN for
real deployments, and describe how to verify relay candidates. Voice/video
users are more likely to encounter restrictive NATs and corporate networks, so
this belongs in the media quickstart rather than only in an advanced section.

## Types and diagnostics

### 11. Expand or supplement `PeerLike`

`PeerLike` intentionally exposes only the methods used internally, but
`simplePeer` is publicly available and documented as an escape hatch. Its type
therefore prevents legitimate media use without a cast.

Either:

- Return `SimplePeer.Instance | null` from `simplePeer`, or
- Keep the restricted internal type but provide a separately typed
  `unsafeSimplePeer`/`rawPeer` escape hatch, or
- Add media methods and lifecycle events to the wrapper so most consumers never
  need the raw peer.

The third option gives the best stable public API; a typed escape hatch still
helps advanced applications.

### 12. Add media-focused diagnostics

Optional diagnostics would make field failures much easier to understand:

- Current internal peer generation.
- Negotiation in progress/queued/stable state.
- Local and remote track kinds and ready states, without exposing labels unless
  explicitly requested.
- ICE connection state and selected candidate type.
- A safe `getStats()` passthrough or summarized connection-quality API.

Avoid logging SDP, device labels, IP addresses, or other sensitive details by
default.

## Suggested tests

The library's fake WebSocket and fake `simple-peer` suite can cover most media
lifecycle behavior without real browser media:

1. A host adds a track before any guest joins; it is attached when the peer is
   built.
2. A guest adds a track before and after `joinRoom` resolves.
3. Multiple tracks are attached exactly once to one peer generation.
4. Tracks are reattached exactly once after `peer-reset` rebuilds the peer.
5. Removed or ended tracks are not reattached.
6. Replacing a track updates both the active peer and the retained registry.
7. Remote `stream` and `track` events preserve their payloads and ordering.
8. Closing the room clears retained media references without stopping tracks
   owned by the caller.
9. Simultaneous media changes on both peers complete renegotiation without
   corrupting application data frames.

A smaller browser integration suite using Playwright and fake media devices
would then validate microphone/camera negotiation, autoplay handling, and
reconnection in Chromium, Firefox, and WebKit.

## Recommended minimal first release

A focused media-support release could deliver most of the value with four
changes:

1. Add wrapper-level `addTrack`, `removeTrack`, and `replaceTrack` methods.
2. Retain desired tracks and reattach them after internal peer construction or
   rebuild.
3. Add one audio-only and one camera/microphone example to the README.
4. Add lifecycle tests for tracks registered before joining and after a peer
   reset.

That would let applications build reliable voice and video without depending on
`simplePeer` construction timing, casts, or custom track registries, while
keeping media capture and playback policy in application code.
