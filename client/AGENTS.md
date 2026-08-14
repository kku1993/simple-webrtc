# AGENTS.md — simple-webrtc-client

Zero-dependency TypeScript WebRTC client speaking the signaling protocol in
`docs/DESIGN.md`. Source lives in `client/src/`, tests in `client/test/`.

The client owns its WebRTC engine (`src/rtc/`) rather than depending on
`simple-peer`; see `docs/RTC_ENGINE_PLAN.md` for the rationale and the audit
that drove it.

## Commands (run from `client/`)

- `npm install` — install deps. NOTE: `esbuild` (a transitive dep of `tsx`) has
  a postinstall script that npm's `allow-scripts` blocks by default; run
  `npm install-scripts approve esbuild` once so `tsx` works for tests.
- `npm run build` — `tsc -p tsconfig.build.json` → emits ESM + declarations to `dist/`.
- `npm run typecheck` — `tsc -p tsconfig.json --noEmit` (covers `src` + `test`).
- `npm run lint` — `eslint .` (flat config, type-checked).
- `npm test` — `node --test --import tsx test/*.test.ts` (197 tests; uses a
  fake WebSocket, a fake peer, and a fake `RTCPeerConnection` — no browser and
  no native WebRTC needed).

## Tooling notes

- TypeScript 5.9.3 (typescript-eslint 8.x does not yet support TS 7).
- eslint 9.39.5 flat config (`eslint.config.js`); type-checked rules via
  `projectService`. `tsconfig.json` includes `src` + `test` (noEmit) for lint;
  `tsconfig.build.json` emits only `src`.
- Imports use the NodeNext `.js` extension convention.
- **No runtime dependencies.** Keep it that way.
- The client uses the global `WebSocket` (browsers and Node >= 22); no `ws`
  dependency is required.
- `tsconfig.build.json` sets `types: []`, so `src/` must not reference Node
  globals (`require`, `Buffer`, `process`).
- RTC is browser-only for supported purposes. Node support covers the
  signaling/state-machine layer, which the fakes-based suite exercises.
  `RtcPeerOptions.rtcImpl` is the single injection point if someone wants
  `node-datachannel`/`wrtc`; it is untested.

## Architecture

Two layers:

1. `src/rtc/` — the WebRTC engine. `RtcPeer` owns one `RTCPeerConnection`,
   negotiation, ICE, media, and all data channels. Knows nothing about rooms or
   the signaling protocol.
2. `src/peer-connection.ts` — `PeerConnection`, the protocol state machine. Owns
   rooms, epochs, reconnect, and the desired-media / channel-handle registries.

`src/manifest.ts` sits beside both: it turns an operator-authored JSON config
into a signaling URL (weighted random choice for a host creating a room,
room-id prefix lookup for a guest joining one). The manifest is the shard
directory only — ICE/TURN configuration is not carried there. The server mints
short-lived TURN credentials per connection and returns them in the handshake
responses (`iceServers` field); `PeerConnection` captures them and applies them
to every peer generation. A connection pins its shard for its lifetime — a
reconnect must return to the process holding the room. `PeerConnection`'s `url`
option is sugar for a one-wildcard-shard manifest, and shard resolution stays
synchronous whenever the manifest is already in hand, so a statically configured
client opens its socket in the same tick it was asked to.

### Engine (`src/rtc/`)

- `peer.ts` — `RtcPeer`: lifecycle, event surface, control-channel routing.
- `negotiation.ts` — offer/answer. Strictly initiator-driven: only the host
  offers, the guest signals `{t:'renegotiate'}` to ask for one. Because roles
  are fixed by the protocol, glare is impossible and there is no rollback path.
  Requests are batched on a microtask and one queued renegotiation runs when
  signaling returns to `stable`.
- `ice.ts` — trickle plus the pending-candidate queue (candidates routinely
  arrive before the description they belong to). A candidate that will not apply
  is warned about, never fatal.
- `channels.ts` / `channel-handle.ts` — the channel registry. **The initiator
  creates every declared channel; the responder binds by label.** No stream-id
  arithmetic, no collision window, and configuration mismatch is structurally
  impossible. `DataChannelHandle` identity is stable across peer rebuilds
  because `PeerConnection` owns the handle map and passes it into every
  generation.
- `media.ts` — sender map keyed by `(track, stream)`, and inbound track/stream
  dedup. `stream` is emitted on a microtask so every track is attached first.
- `signal.ts` — the peer-to-peer wire format. Explicitly discriminated on `t`,
  with unknown frames ignored rather than fatal.
- Reserved channel labels: `__sps_ctrl` (renegotiation) and `__sps_data`
  (backs `send()` / the `data` event). Control traffic has its own channel, so
  application data is never inspected for control frames.

### Protocol state machine (`peer-connection.ts`)

- Host: `createRoom()` → wait for `guest-joined` → build `RtcPeer({initiator:true})`.
- Guest: `joinRoom()` → build `RtcPeer({initiator:false})` immediately.
- `signal` events are sent as `{type:'signal', seq, data}`; `signal-response` is
  fed to `peer.signal()`.
- On `peer.connect`, sends `peer-connected`. Close `4200` (room-idle-close) is
  success — no reconnect. Subsequent renegotiation goes peer-to-peer over the
  engine's control channel.
- On a retryable socket close, reconnects with `rejoin-room` and a fresh epoch
  (full-jitter backoff). `peer-reset` (other side's epoch changed) rebuilds the
  peer; roles/initiator never change. Data channel *handles* survive the
  rebuild; the underlying channels do not.
- `rejoin(session)` resumes a persisted session after a page reload.
- State `{ roomId, role, rejoinToken, hostEpoch, guestEpoch }` is persisted via
  `RoomSessionStore` (browser `sessionStorage` or in-memory).

`PeerConnection` accepts injectable `transportFactory` and `peerFactory` options
so the state machine is unit-testable with no engine at all. To test the real
engine instead, pass `rtcImpl` and use the doubles in `test/rtc-fakes.ts`.

## Testing notes

- `test/fakes.ts` — fake WebSocket + `FakePeer` (an `RtcPeerLike` stub) for
  protocol tests.
- `test/manifest.test.ts` — manifest parsing, weighted selection, room-id →
  shard lookup, the caching loader (stale-on-refresh-failure, fallback,
  single-flight), and the `PeerConnection` wiring.
- `test/rtc-fakes.ts` — `FakePeerConnection` / `FakeDataChannel` for engine
  tests. The fake maintains a real `signalingState` machine, so negotiation
  ordering and queueing are genuinely exercised.
- Engine behaviors that are load-bearing and easy to regress each have a named
  test: pending-candidate flush, negotiation batching, one-queued-renegotiation,
  non-fatal candidate rejection, inbound stream dedup + deferred emit, refusal
  to re-add a removed track, and the both-conditions `connect` gate.
- Not covered: real ICE/NAT behavior. There is no browser test infrastructure
  yet — see `docs/RTC_ENGINE_PLAN.md` phase 5.
