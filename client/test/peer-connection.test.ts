import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PeerConnection, SignalingError } from '../src/peer-connection.js';
import type { PeerConnectionEvents, PeerStatus } from '../src/peer-connection.js';
import { CloseCode, ErrorCode } from '../src/types.js';
import { createFakeHarness, type FakeHarness, type FakePeer } from './fakes.js';

// --- helpers ---------------------------------------------------------------

/** Yield so queued microtasks (e.g. FakeWebSocket auto-open) run. */
function flush(): Promise<void> {
  return new Promise((r) => setImmediate(r));
}

interface ParsedMsg {
  type: string;
  [k: string]: unknown;
}

function sentMessages(h: FakeHarness): ParsedMsg[] {
  return h.ws.sent.map((s) => JSON.parse(s) as ParsedMsg);
}

function lastOf(h: FakeHarness, type: string): ParsedMsg | undefined {
  return [...sentMessages(h)].reverse().find((m) => m.type === type);
}

function waitForMessage(h: FakeHarness, type: string, timeoutMs = 1000): Promise<ParsedMsg> {
  return new Promise((resolve, reject) => {
    const start = Date.now();
    const check = (): void => {
      const m = lastOf(h, type);
      if (m) return resolve(m);
      if (Date.now() - start > timeoutMs) return reject(new Error(`timeout waiting for ${type}`));
      setImmediate(check);
    };
    check();
  });
}

interface Recorded {
  status: PeerStatus[];
  events: { event: keyof PeerConnectionEvents; arg: unknown }[];
}

function record(conn: PeerConnection): Recorded {
  const rec: Recorded = { status: [], events: [] };
  conn.on('status', (s) => rec.status.push(s));
  for (const e of [
    'state',
    'guest-joined',
    'connect',
    'data',
    'peer-disconnected',
    'peer-rejoined',
    'peer-reset',
    'socket-released',
    'room-closed',
    'room-expired',
    'server-shutdown',
    'error',
    'close',
  ] as const) {
    conn.on(e, (arg) => rec.events.push({ event: e, arg }));
  }
  return rec;
}

function makeConn(h: FakeHarness, opts: { maxReconnectAttempts?: number } = {}) {
  return new PeerConnection({
    url: 'wss://signal.example/v1/signal',
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
    maxReconnectAttempts: opts.maxReconnectAttempts,
  });
}

const ISO_FUTURE = new Date(Date.now() + 12 * 3600 * 1000).toISOString();
const ISO_SOON = new Date(Date.now() + 600 * 1000).toISOString();

function createRoomResponse(roomId: string, rejoinToken: string, hostEpoch: string): ParsedMsg {
  return {
    type: 'create-room-response',
    roomId,
    role: 'host',
    rejoinToken,
    peerDeadlineAt: ISO_SOON,
    peerDeadlineInSeconds: 600,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
    // carry the host epoch on the wire is not part of the protocol, but the
    // test harness uses it to verify state; the client ignores unknown fields.
    hostEpoch,
  };
}

function joinRoomResponse(roomId: string, rejoinToken: string, guestEpoch: string, hostEpoch: string | null): ParsedMsg {
  return {
    type: 'join-room-response',
    roomId,
    role: 'guest',
    rejoinToken,
    hostConnected: true,
    hostEpoch,
    guestEpoch,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
  };
}

// --- tests -----------------------------------------------------------------

test('host: createRoom → guest-joined → signal relay → connect → peer-connected', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const created = conn.createRoom();
  const createMsg = await waitForMessage(h, 'create-room');
  const hostEpoch = createMsg['hostEpoch'] as string;
  h.ws.receiveMessage(createRoomResponse('room-1', 'tok-host', hostEpoch));
  const result = await created;

  assert.equal(result.roomId, 'room-1');
  assert.equal(conn.role, 'host');
  assert.equal(conn.roomState?.hostEpoch, hostEpoch);
  assert.equal(conn.roomState?.guestEpoch, null);
  // Host must NOT construct SimplePeer before guest-joined.
  assert.equal(h.peers.length, 0);
  assert.equal(conn.currentStatus, 'connecting');

  // Guest joins.
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-1' });
  await flush();
  assert.equal(h.peers.length, 1);
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.initiator, true);
  assert.ok(rec.events.some((e) => e.event === 'guest-joined'));
  assert.equal(conn.roomState?.guestEpoch, 'guest-epoch-1');

  // peer emits a signal → client sends {type:'signal', seq:1, data}
  peer.emitSignal({ v: 1, t: 'candidate', candidate: { candidate: 'ice-1' } });
  await flush();
  const sig = lastOf(h, 'signal');
  assert.ok(sig, 'expected a signal message');
  assert.equal(sig['seq'], 1);
  assert.equal(typeof sig['data'], 'string');

  // server delivers a signal-response → peer.signal called
  h.ws.receiveMessage({
    type: 'signal-response',
    fromRole: 'guest',
    fromEpoch: 'guest-epoch-1',
    seq: 1,
    data: JSON.stringify({ type: 'candidate', candidate: { candidate: 'ice-2' } }),
    receivedAt: new Date().toISOString(),
  });
  await flush();
  assert.equal(peer.signals.length, 1);

  // peer connects → client sends peer-connected and emits 'connect'
  peer.emitConnect();
  await flush();
  assert.ok(lastOf(h, 'peer-connected'), 'expected peer-connected message');
  assert.ok(rec.events.some((e) => e.event === 'connect'));
  assert.equal(conn.currentStatus, 'connected');
  assert.equal(conn.connected, true);

  conn.destroy();
});

test('host: room-idle-close then close 4200 is treated as success (no reconnect)', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-2', 'tok-host', 'host-epoch-2'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-2' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.emitConnect();
  await flush();

  h.ws.receiveMessage({ type: 'room-idle-close', reason: 'peer-connected' });
  await flush();
  h.ws.closeFromServer(CloseCode.RELEASED_AFTER_PEER_CONNECTED, 'released');
  await flush();

  // No rejoin-room should be sent.
  assert.equal(lastOf(h, 'rejoin-room'), undefined);
  assert.equal(conn.currentStatus, 'connected');
  conn.destroy();
});

test('guest: joinRoom builds the non-initiator peer immediately', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  // Use a canonical id without O/I/L so normalization is a no-op here; the
  // normalization behavior itself is covered by the dedicated tests below.
  const joined = conn.joinRoom({ roomId: 'ta0003' });
  await waitForMessage(h, 'join-room');
  const joinMsg = lastOf(h, 'join-room');
  assert.ok(joinMsg);
  assert.equal(joinMsg['roomId'], 'ta0003');
  assert.equal(typeof joinMsg['guestEpoch'], 'string');

  h.ws.receiveMessage(joinRoomResponse('ta0003', 'tok-guest', joinMsg['guestEpoch'] as string, 'host-epoch-3'));
  const result = await joined;

  assert.equal(result.hostConnected, true);
  assert.equal(conn.role, 'guest');
  assert.equal(conn.roomState?.hostEpoch, 'host-epoch-3');
  // Guest builds its peer right away.
  assert.equal(h.peers.length, 1);
  assert.equal((h.peers[0] as FakePeer).initiator, false);
  conn.destroy();
});

test('guest: joinRoom normalizes the room id via Crockford base32 fuzzy decoding before sending', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  // User-entered id with uppercase + fuzzy-equivalent chars (O→0, I→1, L→1).
  // The client must NOT reject this; it normalizes and lets the backend
  // validate. See docs/ROOM_ID_SPEC.md §"Frontend handling".
  const joined = conn.joinRoom({ roomId: 'TOl234' });
  await waitForMessage(h, 'join-room');
  const joinMsg = lastOf(h, 'join-room');
  assert.ok(joinMsg);
  assert.equal(joinMsg['roomId'], 't01234');
  assert.equal(typeof joinMsg['guestEpoch'], 'string');

  // The server responds with the canonical form; the client stores that, not
  // the user-entered string.
  h.ws.receiveMessage(joinRoomResponse('t01234', 'tok-guest', joinMsg['guestEpoch'] as string, 'host-epoch-n'));
  const result = await joined;
  void result;

  assert.equal(conn.roomState?.roomId, 't01234');
  conn.destroy();
});

test('guest: joinRoom passes through malformed ids without rejecting', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  // Wrong length and a separator — still sent to the backend, which owns
  // validation/rejection per docs/ROOM_ID_SPEC.md §"Frontend handling".
  const joined = conn.joinRoom({ roomId: 'TA-000' });
  await waitForMessage(h, 'join-room');
  const joinMsg = lastOf(h, 'join-room');
  assert.ok(joinMsg);
  assert.equal(joinMsg['roomId'], 'ta-000');

  // Reject via an error-response so the promise settles and the test cleans up.
  h.ws.receiveMessage({
    type: 'error-response',
    requestId: joinMsg['requestId'] as string,
    errorCode: ErrorCode.ROOM_CLOSED,
    message: 'no such room',
    retryable: false,
  });
  await assert.rejects(joined, (err: unknown) => err instanceof SignalingError);
  conn.destroy();
});

test('close() sends close-room, destroys the peer, and emits close', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-4', 'tok-host', 'host-epoch-4'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-4' });
  await flush();
  const peer = h.peers[0] as FakePeer;

  conn.close('done');
  await flush();

  assert.ok(lastOf(h, 'close-room'), 'expected close-room message');
  assert.equal(peer.destroyed, true);
  assert.ok(rec.events.some((e) => e.event === 'close'));
  conn.destroy();
});

test('handshake error-response rejects the createRoom promise', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  const req = lastOf(h, 'create-room');
  h.ws.receiveMessage({
    type: 'error-response',
    requestId: req!['requestId'] as string,
    errorCode: ErrorCode.RATE_LIMITED,
    message: 'slow down',
    retryable: true,
    retryAfterMs: 100,
  });
  await assert.rejects(created, (err: unknown) => {
    assert.ok(err instanceof SignalingError);
    assert.equal(err.code, ErrorCode.RATE_LIMITED);
    assert.equal(err.retryAfterMs, 100);
    return true;
  });
  conn.destroy();
});

test('peer-reset event rebuilds the SimplePeer', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-5', 'tok-host', 'host-epoch-5'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-5' });
  await flush();
  const firstPeer = h.peers[0] as FakePeer;
  assert.equal(h.peers.length, 1);

  // Other side reattached with a new epoch.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-epoch-5b' });
  await flush();

  assert.equal(firstPeer.destroyed, true);
  assert.equal(h.peers.length, 2);
  assert.equal((h.peers[1] as FakePeer).initiator, true); // host stays initiator
  assert.ok(rec.events.some((e) => e.event === 'peer-reset'));
  assert.equal(conn.roomState?.guestEpoch, 'guest-epoch-5b');
  conn.destroy();
});

test('peer-reset rebuild does not schedule a reconnect on the attached socket', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);

  const destroyed: { generation: number; reason: string }[] = [];
  conn.on('peer-destroyed', (e) => destroyed.push(e));

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-5c', 'tok-host', 'host-epoch-5c'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-5c' });
  await flush();
  (h.peers[0] as FakePeer).emitConnect();
  await flush();
  assert.equal(conn.currentStatus, 'connected');

  // The other side reattached with a new epoch. Destroying our peer to rebuild
  // makes it emit `close`, which must NOT be mistaken for "the P2P connection
  // died": our signaling socket is still attached, so the rejoin-room it would
  // schedule is rejected by the server with 1004 / close 4001.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-epoch-5c-NEW' });
  await flush();

  assert.equal(conn.currentStatus, 'signaling');
  assert.equal(h.peers.length, 2);
  assert.equal((h.peers[1] as FakePeer).destroyed, false);
  // Exactly one peer-destroyed, carrying the real reason (not 'close').
  assert.deepEqual(destroyed, [{ generation: 1, reason: 'rebuild' }]);

  // Give the reconnect backoff (attempt 0, < 500ms) time to fire.
  await new Promise((r) => setTimeout(r, 700));
  assert.equal(lastOf(h, 'rejoin-room'), undefined);
  assert.equal(conn.currentStatus, 'signaling');
  conn.destroy();
});

test('peer dying on its own still triggers a reconnect', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);

  const destroyed: { generation: number; reason: string }[] = [];
  conn.on('peer-destroyed', (e) => destroyed.push(e));

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-5d', 'tok-host-5d', 'host-epoch-5d'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-5d' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.emitConnect();
  await flush();

  // ICE failure: the peer closes without anyone asking it to.
  peer.emitClose();
  await flush();

  assert.deepEqual(destroyed, [{ generation: 1, reason: 'close' }]);
  assert.equal(conn.currentStatus, 'reconnecting');
  const rejoin = await waitForMessage(h, 'rejoin-room', 2000);
  assert.equal(rejoin['rejoinToken'], 'tok-host-5d');
  conn.destroy();
});

test('a rejoin rejected as "already attached" does not tear down the session', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const destroyed: { generation: number; reason: string }[] = [];
  conn.on('peer-destroyed', (e) => destroyed.push(e));

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-5e', 'tok-host-5e', 'host-epoch-5e'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-5e' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.emitConnect();
  await flush();

  // Force a rejoin on a socket that is still attached, then have the server
  // answer the way it now does: a non-fatal UNEXPECTED_STATE, no close.
  peer.emitClose();
  const rejoin = await waitForMessage(h, 'rejoin-room', 2000);
  h.ws.receiveMessage({
    type: 'error-response',
    requestId: rejoin['requestId'] as string,
    errorCode: ErrorCode.UNEXPECTED_STATE,
    message: 'connection already attached',
    retryable: false,
  });
  await flush();

  // The session survives: no terminal error, no close, socket still open.
  assert.equal(conn.currentStatus, 'waiting-for-peer');
  assert.ok(!rec.events.some((e) => e.event === 'error'));
  assert.ok(!rec.events.some((e) => e.event === 'close'));
  assert.equal(h.ws.readyState, 1); // still OPEN
  assert.deepEqual(destroyed, [{ generation: 1, reason: 'close' }]);
  conn.destroy();
});

test('reconnect: retryable close triggers a rejoin-room with a fresh epoch', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-6', 'tok-host-6', 'host-epoch-6'));
  await created;

  // A retryable close. The client must reconnect with rejoin-room.
  h.ws.closeFromServer(CloseCode.RATE_LIMITED, 'rate limited');
  await flush();
  assert.equal(conn.currentStatus, 'reconnecting');

  // Wait for the scheduled rejoin-room (full-jitter backoff, attempt 0 < 500ms).
  await waitForMessage(h, 'rejoin-room', 2000);
  const rejoin = lastOf(h, 'rejoin-room');
  assert.ok(rejoin);
  assert.equal(rejoin['rejoinToken'], 'tok-host-6');
  assert.equal(typeof rejoin['epoch'], 'string');
  assert.notEqual(rejoin['epoch'], 'host-epoch-6'); // fresh epoch on reconnect

  // Server rebuilds the room from the token.
  h.ws.receiveMessage({
    type: 'rejoin-room-response',
    roomId: 'room-6',
    role: 'host',
    recreated: true,
    peerConnected: false,
    hostEpoch: rejoin['epoch'] as string,
    guestEpoch: null,
    peerDeadlineAt: ISO_SOON,
    peerDeadlineInSeconds: 600,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
  });
  await flush();
  assert.equal(conn.currentStatus, 'waiting-for-peer');
  conn.destroy();
});

test('rejoin with a changed peer epoch rebuilds the SimplePeer (peer-reset)', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const created = conn.createRoom();
  const createMsg = await waitForMessage(h, 'create-room');
  const hostEpoch = createMsg['hostEpoch'] as string;
  h.ws.receiveMessage(createRoomResponse('room-6b', 'tok-host-6b', hostEpoch));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-6b' });
  await flush();
  const firstPeer = h.peers[0] as FakePeer;
  assert.equal(h.peers.length, 1);

  // Retryable close → reconnect with rejoin-room.
  h.ws.closeFromServer(CloseCode.RATE_LIMITED, 'rate limited');
  await waitForMessage(h, 'rejoin-room', 2000);
  const rejoin = lastOf(h, 'rejoin-room')!;

  // Server did NOT recreate, peer is connected, but the guest reattached with a
  // NEW epoch. The host must destroy & rebuild its SimplePeer.
  h.ws.receiveMessage({
    type: 'rejoin-room-response',
    roomId: 'room-6b',
    role: 'host',
    recreated: false,
    peerConnected: true,
    hostEpoch: rejoin['epoch'] as string,
    guestEpoch: 'guest-epoch-6b-CHANGED',
    peerDeadlineAt: null,
    peerDeadlineInSeconds: null,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
  });
  await flush();

  assert.equal(firstPeer.destroyed, true);
  assert.equal(h.peers.length, 2);
  assert.equal((h.peers[1] as FakePeer).initiator, true); // host stays initiator
  assert.ok(rec.events.some((e) => e.event === 'peer-reset'));
  assert.equal(conn.roomState?.guestEpoch, 'guest-epoch-6b-CHANGED');
  conn.destroy();
});

test('replaced (4400) is terminal and does not reconnect', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-7', 'tok-host', 'host-epoch-7'));
  await created;

  h.ws.closeFromServer(CloseCode.REPLACED, 'another tab took the slot');
  await flush();
  assert.equal(lastOf(h, 'rejoin-room'), undefined);
  assert.equal(conn.currentStatus, 'error');
  assert.ok(rec.events.some((e) => e.event === 'close'));
  conn.destroy();
});

test('room-closed (4013) is terminal', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const rec = record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-8', 'tok-host', 'host-epoch-8'));
  await created;

  h.ws.closeFromServer(CloseCode.ROOM_CLOSED, 'closed-by-peer');
  await flush();
  assert.equal(lastOf(h, 'rejoin-room'), undefined);
  assert.ok(rec.events.some((e) => e.event === 'room-closed'));
  assert.equal(conn.currentStatus, 'error');
  conn.destroy();
});

test('renegotiation after socket release is sent over the data channel', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-9', 'tok-host', 'host-epoch-9'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-9' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.emitConnect();
  await flush();

  // Release the socket.
  h.ws.receiveMessage({ type: 'room-idle-close', reason: 'peer-connected' });
  await flush();
  h.ws.closeFromServer(CloseCode.RELEASED_AFTER_PEER_CONNECTED, 'released');
  await flush();

  const controlBefore = peer.controlSignals.length;
  // A new signal after release must go over the control channel, not the ws —
  // and not over the application's data channel either.
  peer.emitSignal({ v: 1, t: 'renegotiate' });
  await flush();
  assert.equal(peer.controlSignals.length, controlBefore + 1);
  assert.deepEqual(peer.controlSignals[peer.controlSignals.length - 1], {
    v: 1,
    t: 'renegotiate',
  });
  assert.equal(peer.sent.length, 0, 'control traffic stays off the data channel');
  // And no new signal frame on the websocket.
  assert.equal(lastOf(h, 'signal'), undefined);
  conn.destroy();
});

test('renegotiation after socket release uses the control channel, not the socket', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  record(conn);

  const created = conn.createRoom();
  await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-10', 'tok-host', 'host-epoch-10'));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-epoch-10' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.emitConnect();
  await flush();

  h.ws.receiveMessage({ type: 'room-idle-close', reason: 'peer-connected' });
  await flush();
  h.ws.closeFromServer(CloseCode.RELEASED_AFTER_PEER_CONNECTED, 'released');
  await flush();

  peer.emitSignal({ v: 1, t: 'renegotiate' });
  await flush();

  assert.deepEqual(peer.controlSignals, [{ v: 1, t: 'renegotiate' }]);
  assert.equal(lastOf(h, 'signal'), undefined, 'nothing on the released socket');
  conn.destroy();
});
