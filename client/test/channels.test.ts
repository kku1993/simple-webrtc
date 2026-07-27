// PeerConnection-level data channel behavior: the declared-channel registry,
// stable handle identity across peer rebuilds, and event forwarding.
//
// Engine-level channel mechanics (allocation, whenClosed policy, backpressure)
// are covered in rtc-channels.test.ts against a fake RTCPeerConnection.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PeerConnection } from '../src/peer-connection.js';
import { DataChannelNotOpenError } from '../src/errors.js';
import { createFakeHarness, type FakeHarness, type FakePeer } from './fakes.js';

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

const ISO_FUTURE = new Date(Date.now() + 12 * 3600 * 1000).toISOString();
const ISO_SOON = new Date(Date.now() + 600 * 1000).toISOString();

function createRoomResponse(hostEpoch: string): ParsedMsg {
  return {
    type: 'create-room-response',
    roomId: 'room-c',
    role: 'host',
    rejoinToken: 'tok-host',
    peerDeadlineAt: ISO_SOON,
    peerDeadlineInSeconds: 600,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
    hostEpoch,
  };
}

function makeConn(
  h: FakeHarness,
  dataChannels?: Record<string, Record<string, unknown>>,
): PeerConnection {
  return new PeerConnection({
    url: 'wss://signal.example/v1/signal',
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
    ...(dataChannels ? { dataChannels } : {}),
  });
}

async function hostCreatesRoom(
  h: FakeHarness,
  dataChannels?: Record<string, Record<string, unknown>>,
): Promise<PeerConnection> {
  const conn = makeConn(h, dataChannels);
  const created = conn.createRoom();
  const msg = await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse(msg['hostEpoch'] as string));
  await created;
  return conn;
}

// ---------------------------------------------------------------------------

test('channel() works before any room exists', () => {
  const h = createFakeHarness();
  const conn = makeConn(h, { chat: {}, cursor: { ordered: false, maxRetransmits: 0 } });

  // Same contract as the media API: callers never need to know whether an
  // internal peer currently exists.
  const chat = conn.channel('chat');
  assert.equal(chat.label, 'chat');
  assert.equal(chat.readyState, 'connecting');
  assert.equal(chat.ordered, true);
  assert.equal(conn.channel('cursor').ordered, false);
  conn.destroy();
});

test('channel() returns the same object every time', () => {
  const h = createFakeHarness();
  const conn = makeConn(h, { chat: {} });
  assert.equal(conn.channel('chat'), conn.channel('chat'));
  conn.destroy();
});

test('an undeclared label throws, a reserved one is rejected', () => {
  const h = createFakeHarness();
  const conn = makeConn(h, { chat: {} });
  assert.throws(() => conn.channel('nope'), /openChannel/);
  assert.throws(() => conn.channel('__sps_ctrl'), /reserved/);
  assert.throws(() => conn.openChannel('__sps_data'), /reserved/);
  conn.destroy();
});

test('a reserved label in the declared set is rejected at construction', () => {
  const h = createFakeHarness();
  assert.throws(() => makeConn(h, { __sps_ctrl: {} }), /reserved/);
});

test('declared channels are handed to every peer generation', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {}, cursor: { ordered: false } });
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();

  const peer = h.peers[0] as FakePeer;
  assert.deepEqual(Object.keys(peer.opts.dataChannels ?? {}).sort(), ['chat', 'cursor']);
  assert.equal(peer.opts.initiator, true, 'the host creates the channels');
  conn.destroy();
});

test('handle identity survives a peer rebuild', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {} });
  const handle = conn.channel('chat');

  // An application listener registered once, before any rebuild.
  const messages: unknown[] = [];
  handle.on('message', (d) => messages.push(d));

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();
  assert.equal(h.peers.length, 1);

  // The guest reattached with a new epoch: the peer is destroyed and rebuilt.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'g2' });
  await flush();
  assert.equal(h.peers.length, 2, 'a new peer generation was built');

  assert.equal(conn.channel('chat'), handle, 'the application reference still works');
  assert.equal(handle.readyState, 'connecting');
  conn.destroy();
});

test('the same handle registry is reused across generations', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {} });
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'g2' });
  await flush();

  const first = h.peers[0] as FakePeer;
  const second = h.peers[1] as FakePeer;
  assert.equal(
    first.opts.channelHandles,
    second.opts.channelHandles,
    'both generations bind into one registry',
  );
  assert.equal(first.opts.channelHandles?.get('chat'), conn.channel('chat'));
  conn.destroy();
});

test('buffered sends survive a rebuild and are not lost', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {} });
  const chat = conn.channel('chat');

  // Reliable channels buffer by default, so a send before the channel opens is
  // held rather than dropped or thrown.
  chat.send('queued');
  assert.equal(chat.queuedCount, 1);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'g2' });
  await flush();

  assert.equal(chat.queuedCount, 1, 'still queued for the new generation');
  conn.destroy();
});

test('unreliable channels throw rather than buffering stale state', () => {
  const h = createFakeHarness();
  const conn = makeConn(h, { cursor: { ordered: false, maxRetransmits: 0 } });
  assert.throws(() => conn.channel('cursor').send('10,20'), DataChannelNotOpenError);
  conn.destroy();
});

test('openChannel before a peer exists registers for the next generation', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const handle = conn.openChannel('late', { ordered: false });
  assert.equal(handle.label, 'late');
  assert.equal(conn.channel('late'), handle);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();

  const peer = h.peers[0] as FakePeer;
  assert.ok(
    Object.keys(peer.opts.dataChannels ?? {}).includes('late'),
    'the channel joins the declared set for the new peer',
  );
  conn.destroy();
});

test('openChannel after a peer exists delegates to it', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();

  const handle = conn.openChannel('runtime', {});
  assert.equal(handle.label, 'runtime');
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.channel('runtime'), handle);
  conn.destroy();
});

test('a channel opened at runtime is recreated after a peer rebuild', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();

  const handle = conn.openChannel('runtime', { ordered: false });

  // Epoch change: the peer is thrown away and rebuilt. A channel the
  // application opened must come back with it, or the handle it still holds
  // would never reopen.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'g2' });
  await flush();

  const second = h.peers[1] as FakePeer;
  assert.ok(
    Object.keys(second.opts.dataChannels ?? {}).includes('runtime'),
    'the runtime channel joined the declared set',
  );
  assert.equal(conn.channel('runtime'), handle, 'and kept its identity');
  conn.destroy();
});

test('channel events are forwarded from the peer', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {} });
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();
  const peer = h.peers[0] as FakePeer;

  const messages: { label: string; data: unknown }[] = [];
  conn.on('channel-message', (m) => messages.push(m));
  peer.emitChannelMessage('chat', 'hi');

  assert.deepEqual(messages, [{ label: 'chat', data: 'hi' }]);
  conn.destroy();
});

test('the channels map exposes every known handle', () => {
  const h = createFakeHarness();
  const conn = makeConn(h, { chat: {}, cursor: { ordered: false } });
  conn.openChannel('extra');

  assert.deepEqual([...conn.channels.keys()].sort(), ['chat', 'cursor', 'extra']);
  // A copy, so callers cannot mutate the registry.
  const snapshot = conn.channels;
  conn.openChannel('another');
  assert.equal(snapshot.has('another'), false);
  conn.destroy();
});

test('close() retires every handle', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {} });
  const chat = conn.channel('chat');

  conn.close('done');
  assert.equal(chat.readyState, 'closed');
  assert.equal(conn.channels.size, 0);
});

test('destroy() retires every handle', () => {
  const h = createFakeHarness();
  const conn = makeConn(h, { chat: {} });
  const chat = conn.channel('chat');
  conn.destroy();
  assert.equal(chat.readyState, 'closed');
});

test('diagnostics are empty without a peer and delegate once one exists', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h, { chat: {} });
  assert.deepEqual(conn.dataChannelDiagnostics, []);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();
  // The stub peer reports no diagnostics; the wrapper must not invent any.
  assert.deepEqual(conn.dataChannelDiagnostics, []);
  conn.destroy();
});

test('a connection with no declared channels still works', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'g1' });
  await flush();

  assert.equal(conn.channels.size, 0);
  assert.deepEqual((h.peers[0] as FakePeer).opts.dataChannels, {});
  conn.destroy();
});
