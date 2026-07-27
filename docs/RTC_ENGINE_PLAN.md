# Plan: replacing `simple-peer` with an in-house RTC engine

Status: **implemented in v1.0.0**, phases 1–4. Phase 5 (browser test
infrastructure) is outstanding — see "Implementation notes" at the end.
Supersedes `docs/MULTI_DATA_CHANNEL_PLAN.md`, whose `negotiated: true`
id-allocation scheme existed only to work around `simple-peer`.

## Verdict

**Do it, and do it directly.** Greenfield removes essentially every expensive part
of this change. The prior version of this plan budgeted roughly half its effort on
compatibility machinery — byte-compatible signal envelopes, dual-implementation
releases, version-skew CI, deprecation aliases. None of that is needed. What
remains is ~1400 lines of source that replaces a dependency doing a job it was
never designed for.

Four facts:

1. **The seam already exists.** `PeerLike` (`client/src/peer-connection.ts:38-66`)
   is a narrow hand-written interface — 11 members — and `simplePeerFactory`
   already injects the implementation. The abstraction usually invented *before* a
   swap like this is already built and exercised by 39 tests.
2. **Most of `simple-peer` is dead weight for our targets.** ~600 of its 1052 lines
   are Node-stream plumbing we never touch or quirk handling for Chrome 58 /
   Safari 11 / React Native WebRTC / `wrtc`.
3. **Our constraints let us be simpler than `simple-peer`, not merely equal.**
   Roles are fixed and asymmetric by protocol (host = initiator, never changes), so
   offer/answer glare is structurally impossible. `PeerConnection` already owns
   epoch-based rebuild, so the engine needs no ICE restart or half-open recovery.
4. **The client becomes zero-runtime-dependency.** `simple-peer` is the only entry
   in `dependencies`. Removing it also removes its 7 transitive deps, including the
   ~50 KB `buffer` polyfill pulled in solely for a Duplex stream path we never use.

The one real cost is that `simple-peer` absorbed a decade of NAT and browser
pathology on our behalf, and we would now own that. Mitigation is browser test
infrastructure, which this repo does not have and which is budgeted as its own
phase below — not a footnote.

## What we actually depend on

The entire consumed surface, from `PeerLike`:

| Member | Used by |
| --- | --- |
| `connected` | `get connected`, `sendSignal`, diagnostics |
| `on('signal')` | `sendSignal` → `{type:'signal', seq, data}` |
| `on('connect')` | status, `peer-connected`, `connect` event |
| `on('data')` | `handlePeerData` — app data + `{kind:'renegotiate'}` frames |
| `on('stream'/'track')` | forwarded as typed events |
| `on('close'/'error')` | reconnect trigger, `peer-destroyed` |
| `signal(data)` | `handleSignalResponse`, renegotiate frames |
| `send(data)` | renegotiate frames only |
| `destroy()` | `destroyPeer` |
| `addTrack` / `removeTrack` / `replaceTrack` / `addStream` / `removeStream` | desired-media registry |
| `getStats()` | diagnostics passthrough |

Eleven members. Everything else `simple-peer` exposes — the Duplex stream
interface, `bufferSize`, `write`/`pipe`, `addTransceiver`, `localAddress`/
`remotePort`, `iceStateChange` — is unused.

## Audit of `simple-peer@9.11.1`

| Area | Lines (approx) | Disposition |
| --- | --- | --- |
| Node `readable-stream` Duplex plumbing (`_write`, `_read`, `_onFinish`, `_chunk`/`_cb` backpressure) | ~150 | **Drop.** Unused. Source of the `buffer` and `readable-stream` deps. |
| `getStats()` normalization for callback APIs, `googCandidatePair`, `googRemoteAddress`, Firefox `ipAddress`/`portNumber` (`:727-800`) | ~90 | **Drop.** Targets Chrome ≤57, Firefox ≤52. |
| `_maybeReady()` + `findCandidatePair()` polling for `localAddress`/`remoteAddress`/`remotePort` (`:776-900`) | ~130 | **Drop.** Pure diagnostics — and it *gates the `connect` event*, re-polling every 100 ms until a candidate pair appears. Dropping it makes `connect` fire sooner. |
| `wrtc` injection + `get-browser-rtc`, `_isReactNativeWebrtc` | ~30 | **Reduce** to one injection point (see "Runtime targets"). |
| `allowHalfTrickle` / `filterTrickle` SDP munging | ~40 | **Drop.** We always trickle. |
| `_requestMissingTransceivers` Safari null-`mid` HACK (`:648-658`) | ~12 | **Drop.** Safari ≥15 sets `mid` correctly. |
| Negotiation: batching, `_isNegotiating`, `_queuedNegotiation`, initiator-driven `{renegotiate:true}` (`:382-425`, `:927-940`) | ~90 | **Keep, port faithfully.** This is the good part. |
| ICE: trickle, `_pendingCandidates` queue, `.local` mDNS tolerance (`:202-240`) | ~70 | **Keep, port faithfully.** |
| Offer/answer create + set (`:607-695`) | ~90 | **Keep, simplify.** |
| Media: `_senderMap`, `addTrack`/`removeTrack`/`replaceTrack`/`addStream` (`:275-380`) | ~110 | **Keep, port faithfully.** |
| `_onTrack` stream dedup + deferred `stream` emit (`:1000-1023`) | ~25 | **Keep, port faithfully.** Subtle, easy to get wrong. |
| Data channel setup (`_setupData`, `:502-535`) | ~35 | **Replace.** The single-channel assumption we are escaping. |

Net: ~450 lines to port or rewrite, ~450 dropped outright, ~150 irrelevant.

## Why our constraints make this simpler than a general-purpose library

- **Fixed roles.** `docs/DESIGN.md` assigns host/guest at room creation and roles
  never change. Offer/answer glare — the hardest part of WebRTC negotiation, and
  the reason "perfect negotiation" exists — cannot occur. We keep `simple-peer`'s
  asymmetric model (the responder asks the initiator to re-offer) because it is
  already the right model here, not as a compromise.
- **Epoch-based rebuild.** `PeerConnection` destroys and rebuilds the peer on epoch
  change (`rebuildPeerWithNewEpoch`, `:1028`). No ICE restart, no connection
  resurrection, no half-open recovery — the layer above throws the peer away. That
  deletes an entire state machine.
- **Modern targets only.** `engines.node >= 22`; browsers with native
  `RTCPeerConnection` and unified-plan. Every quirk hack in the table above targets
  something older.
- **One data channel was never a requirement.** We are building the thing
  `simple-peer` structurally cannot do.

## Design

### Placement

Ship as `client/src/rtc/` inside the existing package. Do **not** start by carving
out a separate npm library — that adds versioning and API-stability obligations
before the design has settled. Extract later only if something outside this repo
wants it.

```
client/src/rtc/
  peer.ts          — RtcPeer: connection lifecycle + public events
  negotiation.ts   — offer/answer, batching, queueing, initiator-driven renegotiate
  ice.ts           — trickle, pending-candidate queue, candidate tolerance
  channels.ts      — multi-channel manager
  media.ts         — sender map, add/remove/replaceTrack, inbound track dedup
  signal.ts        — SignalData wire types + parser
  env.ts           — RTCPeerConnection injection point
```

### Signal wire format — designed, not inherited

`PeerConnection.sendSignal` does `JSON.stringify(data)` into
`{type:'signal', seq, data}` and the server relays it opaquely, so this is a wire
format between two clients. Greenfield means we design it properly rather than
reproducing `simple-peer`'s shape.

The specific wart worth *not* inheriting: `simple-peer` dispatches by **field
presence**, not by `type` (`index.js:202-227`) — candidates arrive as
`{candidate: {...}}` with no `type` at all, and a frame carrying neither `sdp` nor
`candidate` nor `renegotiate` destroys the peer. We use an explicit discriminated
union:

```ts
export type SignalData =
  | { t: 'offer';       sdp: string }
  | { t: 'answer';      sdp: string }
  | { t: 'candidate';   candidate: RTCIceCandidateInit }
  | { t: 'renegotiate' }
  | { t: 'transceiver'; kind: string; init?: RTCRtpTransceiverInit };
```

Add a `v: 1` envelope field now, cheap insurance for the day the greenfield
assumption expires. Unknown `t` values are logged and ignored rather than fatal —
`simple-peer` destroying the connection on an unrecognized frame is precisely the
behavior that makes protocol evolution painful later.

### Data channels — the payoff

Owning `ondatachannel` makes the whole companion-doc workaround evaporate: no
`peer._pc` private-field access, no `negotiated: true`, no deterministic id
allocation, no pinned primary label, no `_setupData` hijack to defend against, no
channel-announcement frame to detect mismatches.

**Allocation rule: the initiator creates every declared channel; the responder
binds by label.** Both sides declare the same set in options; on the initiator each
declaration becomes `createDataChannel(label, spec)`, on the responder each becomes
a pre-registered handle that binds when the matching `ondatachannel` fires. This is
deterministic with no id math and no collision window, and it makes config
mismatch *structurally impossible* — the channel's ordering and reliability come
from the initiator's creation, so the responder cannot disagree. A label declared
only on the responder simply never opens, and we warn.

```ts
const peer = new PeerConnection({
  url: 'wss://signal.example/v1/signal',
  dataChannels: {
    chat:      {},                                     // ordered, reliable (default)
    cursor:    { ordered: false, maxRetransmits: 0 },  // unordered, unreliable
    telemetry: { ordered: false, maxPacketLifeTime: 100 },
  },
});
```

Dynamic channels are first-class from day one rather than a phase 2, because
either side may call `createDataChannel` once SCTP is up and the id pools are
parity-split by DTLS role, so there is no collision:

```ts
const ch = await peer.openChannel('file-transfer-42', { ordered: true });
peer.on('channel', ({ channel }) => { /* remote opened one */ });
```

**Control frames get their own channel.** A reserved `__ctrl` channel, created
first by the initiator, carries renegotiation. Today's `{kind:'renegotiate'}`
frames are interleaved with application bytes on the same channel that
`on('data')` surfaces — `handlePeerData` (`:1154`) emits every chunk to the app
*and* sniffs it for control frames, so an application sending a JSON string with a
`kind` field can collide with the protocol. Splitting them removes that hazard and
lets `on('data')` become a clean per-channel API.

The `DataChannelHandle` design from the companion doc survives unchanged — stable
handle identity across peer rebuilds, `whenClosed: 'throw' | 'buffer' | 'drop'`,
`drain`/`bufferedAmount` backpressure. That it survives the engine swap intact is
good evidence it sat at the right layer.

### The `connect` condition

`connected` becomes `connectionState === 'connected'` **and** the `__ctrl` channel
open. Declared channels open on their own schedule and fire `channel-open`
individually; `connect` does not wait for them. Apps gate sends via `readyState` or
let the per-channel `whenClosed` policy handle it.

### Runtime targets

Decide explicitly rather than inheriting `simple-peer`'s ambiguity: **RTC is
browser-only.** The client's Node support covers the signaling and state-machine
layer, which is what the existing fakes-based suite already assumes. `env.ts` keeps
a single injection point (`rtcImpl?: { RTCPeerConnection: ... }`) so `node-datachannel`
or `wrtc` can be supplied by a consumer, but we do not test or support it.

### What we must not lose

The non-obvious, field-tested details. Each gets a named test:

1. **Pending ICE candidates** buffered until `setRemoteDescription` resolves
   (`:202-208`). Candidates routinely arrive before the answer.
2. **Negotiation batching** via microtask (`:382-395`) — two `addTrack` calls in a
   row must produce one offer, not two.
3. **`_isNegotiating` / `_queuedNegotiation`** (`:403-425`, `:927-940`) — never
   `createOffer` while `signalingState !== 'stable'`; run exactly one queued
   renegotiation afterward.
4. **`.local` / mDNS candidate tolerance** (`:230-240`) — a failing
   `addIceCandidate` must warn, not tear down the connection.
5. **Inbound track dedup + deferred `stream` emit** (`:1000-1023`) — one `stream`
   event per stream id, on a microtask so all its tracks are attached first.
6. **Sender map keyed by `[track, stream]`** (`:293-305`) — re-adding a removed
   track throws rather than producing a dead sender.
7. **`connect` requires both** transport connected **and** control channel open.

## Changes to the existing client

With no compatibility constraint, the `simple-peer` vocabulary comes out of the
public API entirely:

| Current | Becomes |
| --- | --- |
| `PeerConnectionOptions.simplePeer` | `rtc: RtcPeerOptions` |
| `PeerConnectionOptions.simplePeerFactory` | `peerFactory` |
| `PeerLike` | `RtcPeerLike` — same 11 members plus channel methods |
| `get simplePeer()` | `get peer(): RtcPeer \| null` |
| `get rawPeer(): SimplePeer.Instance` | **Deleted.** It existed only because `PeerLike` was a deliberately narrow view of a foreign type; we own the type now. |
| `types.ts:245-251` — `SimplePeer*` namespace re-exports | **Deleted.** Replaced by our own `SignalData` etc. |
| `DataChannelFrame` / `RenegotiateFrame` in `types.ts` | Moves to `rtc/signal.ts`, off the app data path |

Also removed: `simple-peer` and `@types/simple-peer` from `package.json` — which
incidentally resolves the existing `@types/simple-peer@9.11.9` vs
`simple-peer@9.11.1` version mismatch, and the standing drift risk of hand-written
types tracking a JS library.

`PeerConnection`'s protocol state machine, events, media API, storage, and error
surface are untouched. The desired-media registry pattern (`desiredTracks` +
`attachDesiredTracksToPeer`, `:1046`) is exactly what the desired-channel registry
mirrors, and neither changes shape.

Version this **1.0.0** — the API breaks are real, and a greenfield project
renaming its way off a dependency is the right moment to commit to a surface.

## Test strategy

- **Unit (`node:test`, no browser).** A `FakeRTCPeerConnection` implementing the
  spec surface we touch, driven deterministically — negotiation ordering, candidate
  queueing, the seven must-not-lose behaviors, channel lifecycle, handle identity
  across rebuilds. Extends the existing fakes approach; most assertions live here.
- **Integration in a real browser (new infrastructure).** Two `RtcPeer`s in one
  Playwright page over loopback: connect, media, ordered vs unordered channels,
  renegotiation, teardown. **The repo has none of this today — it is its own phase,
  not a footnote.** It is also the only thing standing between us and the ICE
  pathology `simple-peer` used to absorb.
- **Manual matrix.** Chrome/Firefox/Safari, desktop + mobile, and at least one
  symmetric-NAT/TURN path. Automation will not cover this.

## Risks

1. **NAT/ICE pathology we cannot reproduce locally.** The dominant residual risk,
   and the one greenfield does *not* reduce. Mitigated by the browser phase and by
   keeping the engine small enough to reason about.
2. **Browser regressions after we ship.** `simple-peer` absorbed these for us; we
   own them now, on our own cadence. A standing maintenance commitment, not a
   one-time cost.
3. **Scope creep into "a better simple-peer."** The mandate is the 11-member
   contract plus multi-channel. Not simulcast, not transceiver APIs, not
   stream/pipe compatibility. Every line beyond that justifies itself.
4. **`getStats()` shape.** We return the native `RTCStatsReport` rather than
   `simple-peer`'s flattened array — which is what `PeerConnection.getStats()`
   already declares (`:691`), so this is a fix, not a break.

## Phasing and estimate

| Phase | Content | Source | Tests |
| --- | --- | --- | --- |
| 1 | `RtcPeer` core: signal format, negotiation, ICE, connect/close, control channel | ~650 | ~450 |
| 2 | Media: sender map, add/remove/replace, inbound dedup | ~250 | ~200 |
| 3 | Multi-channel: declared + dynamic, handles, backpressure | ~350 | ~300 |
| 4 | `PeerConnection` cutover, rename, dependency removal, docs | ~150 | ~100 |
| 5 | Browser test infrastructure (Playwright) | ~150 | ~400 |

~1550 lines of source, ~1450 of tests. Phases 1-3 are the engine and can land
behind the existing `simplePeerFactory` seam without touching `peer-connection.ts`,
which keeps the suite green throughout; phase 4 is the rename and deletion.

Phase 5 can run in parallel with 1-3 and should not be deferred past phase 4 —
merging the cutover without a real-browser signal is the one sequencing mistake
worth avoiding.

## Documentation follow-ups

`AGENTS.md`, `client/AGENTS.md`, and `README.md` all describe the client as "a
TypeScript client wrapping `simple-peer`," and `client/AGENTS.md` carries tooling
notes about its CJS `export =` interop. All of that goes. `docs/DESIGN.md` is
unaffected — the server protocol does not change, and the `signal.data` field
stays an opaque relayed string.


## Implementation notes (v1.0.0)

Phases 1–4 shipped. What landed, and where the plan changed on contact:

### Delivered

- `client/src/rtc/` — `peer.ts`, `negotiation.ts`, `ice.ts`, `channels.ts`,
  `channel-handle.ts`, `media.ts`, `signal.ts`, `env.ts`, `index.ts`.
- `simple-peer` and `@types/simple-peer` removed. **The client now has zero
  runtime dependencies.**
- 159 tests passing (up from 39), all in Node against fakes.
  `test/rtc-fakes.ts` provides a `FakePeerConnection` with a real
  `signalingState` machine, so negotiation ordering and queueing are genuinely
  exercised rather than stubbed.

### Deviations from the plan

1. **A default application channel was added.** The plan reserved only
   `__sps_ctrl`. In practice `PeerConnection.on('data')` and `send()` are
   existing public API, so a second reserved channel — `__sps_data` — backs
   them. Applications that want more declare their own; the control channel
   still carries renegotiation alone.
2. **Handle stability needed an ownership change.** The plan asserted handles
   survive rebuilds without saying how. Since `RtcPeer` is destroyed on every
   epoch change, it cannot own the handles: `PeerConnection` owns the
   `Map<label, DataChannelHandle>` and passes it into every generation via
   `RtcPeerOptions.channelHandles`. A superseded generation unsubscribes its
   hooks on dispose, so it cannot keep emitting on a shared handle.
3. **`rawPeer` became `peerInstance`,** not deleted. The plan said delete it;
   an escape hatch is still worth having, and `RtcPeer.peerConnection` exposes
   the raw `RTCPeerConnection` for transceiver work.
4. **`simple-peer`'s Firefox deferred-removal hack was kept**, generalized. Any
   `removeTrack` rejection defers the sender until signaling is `stable` rather
   than only `NS_ERROR_UNEXPECTED`. Cheap, and correct regardless of which
   browser is fussy this year.
5. **Dead Node fallbacks in `util.ts` were removed.** They used bare `require`
   in an ESM-only package, so they could never have run; `tsconfig.build.json`
   sets `types: []`, which is why they broke the build. Pre-existing, unrelated
   to the engine, fixed in passing.

### Bug caught during implementation

`PeerConnection.openChannel` registered a runtime-opened channel in the handle
map but not in the declared set, so the channel would not be recreated after an
epoch change — leaving the application holding a handle that never reopened
again. Fixed, with a named test.

### Outstanding: phase 5

There is still no browser test infrastructure, and it remains the only thing
standing between this engine and the NAT/ICE pathology `simple-peer` used to
absorb. The unit suite covers negotiation ordering, candidate queueing, channel
lifecycle, and handle identity; it cannot cover whether a real connection
establishes across a real symmetric NAT. Before this is relied on in
production, phase 5 should be built:

- Two `RtcPeer`s in one Playwright page over loopback: connect, media, ordered
  vs unordered channels, renegotiation, teardown.
- A manual matrix across Chrome/Firefox/Safari, desktop and mobile, with at
  least one TURN-relayed path.
