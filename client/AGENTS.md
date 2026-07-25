# AGENTS.md — peer-client

TypeScript client wrapping `simple-peer` that speaks the signaling protocol in
`docs/DESIGN.md`. Source lives in `client/src/`, tests in `client/test/`.

## Commands (run from `client/`)

- `npm install` — install deps. NOTE: `esbuild` (a transitive dep of `tsx`) has
  a postinstall script that npm's `allow-scripts` blocks by default; run
  `npm install-scripts approve esbuild` once so `tsx` works for tests.
- `npm run build` — `tsc -p tsconfig.build.json` → emits ESM + declarations to `dist/`.
- `npm run typecheck` — `tsc -p tsconfig.json --noEmit` (covers `src` + `test`).
- `npm run lint` — `eslint .` (flat config, type-checked).
- `npm test` — `node --test --import tsx test/*.test.ts` (39 tests; uses fake
  WebSocket + fake simple-peer, no `wrtc` needed).

## Tooling notes

- TypeScript 5.9.3 (typescript-eslint 8.x does not yet support TS 7).
- eslint 9.39.5 flat config (`eslint.config.js`); type-checked rules via
  `projectService`. `tsconfig.json` includes `src` + `test` (noEmit) for lint;
  `tsconfig.build.json` emits only `src`.
- Imports use the NodeNext `.js` extension convention.
- `simple-peer` is a CJS `export =` module; imported as
  `import SimplePeer from 'simple-peer'` (esModuleInterop).
- `ws` is an optional peer dependency (Node only); browsers use native `WebSocket`.

## Architecture

`PeerConnection` (the public wrapper) owns the protocol state machine:

- Host: `createRoom()` → wait for `guest-joined` → build `SimplePeer({initiator:true})`.
- Guest: `joinRoom()` → build `SimplePeer({initiator:false})` immediately.
- `signal` events are sent as `{type:'signal', seq, data}`; `signal-response` is
  fed to `peer.signal()`.
- On `peer.connect`, sends `peer-connected`. Close `4200` (room-idle-close) is
  success — no reconnect. Subsequent renegotiation goes over the data channel
  as `{kind:'renegotiate', signal}`.
- On a retryable socket close, reconnects with `rejoin-room` and a fresh epoch
  (full-jitter backoff). `peer-reset` (other side's epoch changed) rebuilds the
  `SimplePeer`; roles/initiator never change.
- `rejoin(session)` resumes a persisted session after a page reload.
- State `{ roomId, role, rejoinToken, hostEpoch, guestEpoch }` is persisted via
  `RoomSessionStore` (browser `sessionStorage` or in-memory).

`PeerConnection` accepts injectable `transportFactory` and `simplePeerFactory`
options so the state machine is unit-testable without a browser or `wrtc`.
