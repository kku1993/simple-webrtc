import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PeerConnection } from '../src/peer-connection.js';
import { CloseCode } from '../src/types.js';
import {
  createFakeHarness,
  type FakeHarness,
  type FakePeer,
  FakeTrack,
  FakeStream,
  asTrack,
  asStream,
} from './fakes.js';

// --- helpers (mirrors peer-connection.test.ts) -----------------------------

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
    hostEpoch,
  };
}

function joinRoomResponse(
  roomId: string,
  rejoinToken: string,
  guestEpoch: string,
  hostEpoch: string | null,
): ParsedMsg {
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

function makeConn(h: FakeHarness): PeerConnection {
  return new PeerConnection({
    url: 'wss://signal.example/v1/signal',
    transportFactory: h.transportFactory,
    simplePeerFactory: h.simplePeerFactory,
  });
}

async function hostCreatesRoom(h: FakeHarness): Promise<PeerConnection> {
  const conn = makeConn(h);
  const created = conn.createRoom();
  const createMsg = await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-m', 'tok-host', createMsg['hostEpoch'] as string));
  await created;
  return conn;
}

// --- tests -----------------------------------------------------------------

test('host: addTrack before guest-joined is attached when the peer is built', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const track = asTrack(new FakeTrack({ kind: 'audio' }));
  const stream = asStream(new FakeStream());

  // No peer yet — the call must still succeed.
  conn.addTrack(track, stream);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 1);
  assert.equal(h.peers.length, 0);

  // Guest joins → host builds its SimplePeer; the track must be attached once.
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-1' });
  await flush();
  assert.equal(h.peers.length, 1);
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.addTrackCalls.length, 1);
  assert.equal(peer.addTrackCalls[0]!.track, track);
  assert.equal(peer.addTrackCalls[0]!.stream, stream);

  // peer-created fires with generation 1 and the (already-attached) peer.
  const gens: number[] = [];
  conn.on('peer-created', ({ generation }) => gens.push(generation));
  // Already fired during guest-joined; subscribe after to catch the next one.
  assert.equal(conn.peerGeneration, 1);

  conn.destroy();
});

test('guest: addTrack before and after joinRoom resolves is attached exactly once', async () => {
  const h = createFakeHarness();
  const conn = makeConn(h);
  const track1 = asTrack(new FakeTrack({ kind: 'audio' }));
  const track2 = asTrack(new FakeTrack({ kind: 'video' }));
  const stream = asStream(new FakeStream());

  // Before joinRoom resolves.
  conn.addTrack(track1, stream);

  const joined = conn.joinRoom({ roomId: 'room-g' });
  await waitForMessage(h, 'join-room');
  const joinMsg = lastOf(h, 'join-room')!;
  h.ws.receiveMessage(joinRoomResponse('room-g', 'tok-g', joinMsg['guestEpoch'] as string, 'host-ep-g'));
  await joined;
  await flush();

  assert.equal(h.peers.length, 1);
  const peer = h.peers[0] as FakePeer;
  // track1 was attached during peer construction.
  assert.equal(peer.addTrackCalls.length, 1);
  assert.equal(peer.addTrackCalls[0]!.track, track1);

  // After joinRoom resolves: add a second track. It must be attached to the
  // existing peer exactly once.
  conn.addTrack(track2, stream);
  assert.equal(peer.addTrackCalls.length, 2);
  assert.equal(peer.addTrackCalls[1]!.track, track2);

  // Re-adding the same track must NOT double-attach.
  conn.addTrack(track1, stream);
  assert.equal(peer.addTrackCalls.length, 2);

  conn.destroy();
});

test('multiple tracks are attached exactly once to one peer generation', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const tracks = [
    asTrack(new FakeTrack({ kind: 'audio' })),
    asTrack(new FakeTrack({ kind: 'video' })),
    asTrack(new FakeTrack({ kind: 'audio' })),
  ];
  for (const t of tracks) conn.addTrack(t, stream);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-2' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.addTrackCalls.length, tracks.length);
  // Calling addTrack again on the same peer must not re-attach.
  for (const t of tracks) conn.addTrack(t, stream);
  assert.equal(peer.addTrackCalls.length, tracks.length);
  conn.destroy();
});

test('tracks are reattached exactly once after peer-reset rebuilds the peer', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const track = asTrack(new FakeTrack({ kind: 'audio' }));
  conn.addTrack(track, stream);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-3' });
  await flush();
  const firstPeer = h.peers[0] as FakePeer;
  assert.equal(firstPeer.addTrackCalls.length, 1);

  // Other side reattached with a new epoch → wrapper rebuilds the SimplePeer.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-ep-3b' });
  await flush();

  assert.equal(firstPeer.destroyed, true);
  assert.equal(h.peers.length, 2);
  const secondPeer = h.peers[1] as FakePeer;
  // The live desired track must be re-attached to the new generation exactly once.
  assert.equal(secondPeer.addTrackCalls.length, 1);
  assert.equal(secondPeer.addTrackCalls[0]!.track, track);
  // peer-destroyed fires for the old generation with reason 'rebuild'.
  conn.destroy();
});

test('removed and ended tracks are not reattached', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const liveTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  const removedTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  const endedTrack = asTrack(new FakeTrack({ kind: 'video' }));

  conn.addTrack(liveTrack, stream);
  conn.addTrack(removedTrack, stream);
  conn.addTrack(endedTrack, stream);

  // Remove one track before any peer exists.
  conn.removeTrack(removedTrack, stream);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 2);

  // End one track before any peer exists.
  (endedTrack as unknown as FakeTrack).stop();

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-4' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  // Only the live, non-removed track should be attached.
  assert.equal(peer.addTrackCalls.length, 1);
  assert.equal(peer.addTrackCalls[0]!.track, liveTrack);
  // The ended track should have been pruned from the registry.
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 1);

  // After a peer-reset, the ended/removed tracks must still not re-attach.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-ep-4b' });
  await flush();
  const secondPeer = h.peers[1] as FakePeer;
  assert.equal(secondPeer.addTrackCalls.length, 1);
  assert.equal(secondPeer.addTrackCalls[0]!.track, liveTrack);
  conn.destroy();
});

test('replaceTrack updates the active peer and the retained registry', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const oldTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  const newTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  conn.addTrack(oldTrack, stream);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-5' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.addTrackCalls.length, 1);

  // Replace on the active peer.
  conn.replaceTrack(oldTrack, newTrack, stream);
  assert.equal(peer.replaceTrackCalls.length, 1);
  assert.equal(peer.replaceTrackCalls[0]!.oldTrack, oldTrack);
  assert.equal(peer.replaceTrackCalls[0]!.newTrack, newTrack);

  // The registry should now point at the new track, so a rebuild re-attaches it.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-ep-5b' });
  await flush();
  const secondPeer = h.peers[1] as FakePeer;
  assert.equal(secondPeer.addTrackCalls.length, 1);
  assert.equal(secondPeer.addTrackCalls[0]!.track, newTrack);
  // The old track must not be re-attached.
  assert.ok(
    !secondPeer.addTrackCalls.some((c) => c.track === oldTrack),
    'old track should not be re-attached after replaceTrack',
  );
  conn.destroy();
});

test('replaceTrack with no peer updates the registry and attaches on next build', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const oldTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  const newTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  conn.addTrack(oldTrack, stream);
  // No peer yet — replaceTrack must update the registry without throwing.
  conn.replaceTrack(oldTrack, newTrack, stream);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 1);
  assert.equal(conn.mediaDiagnostics.desiredTracks[0]!.id, (newTrack as unknown as FakeTrack).id);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-6' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.addTrackCalls.length, 1);
  assert.equal(peer.addTrackCalls[0]!.track, newTrack);
  conn.destroy();
});

test('remote stream and track events preserve their payloads', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-7' });
  await flush();
  const peer = h.peers[0] as FakePeer;

  const streams: MediaStream[] = [];
  const trackEvents: { track: MediaStreamTrack; stream: MediaStream }[] = [];
  conn.on('stream', (s) => streams.push(s));
  conn.on('track', (e) => trackEvents.push(e));

  const remoteStream = asStream(new FakeStream());
  const remoteTrack = asTrack(new FakeTrack({ kind: 'audio' }));
  peer.emitTrack(remoteTrack, remoteStream);
  peer.emitStream(remoteStream);
  await flush();

  assert.equal(trackEvents.length, 1);
  assert.equal(trackEvents[0]!.track, remoteTrack);
  assert.equal(trackEvents[0]!.stream, remoteStream);
  assert.equal(streams.length, 1);
  assert.equal(streams[0], remoteStream);
  conn.destroy();
});

test('close() clears retained media references without stopping caller tracks', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const track = asTrack(new FakeTrack({ kind: 'audio' }));
  conn.addTrack(track, stream);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-8' });
  await flush();
  const peer = h.peers[0] as FakePeer;

  conn.close('done');
  await flush();

  assert.equal(peer.destroyed, true);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 0);
  assert.equal(conn.currentLocalStream, null);
  // The wrapper must NOT stop the caller's track.
  assert.equal((track as unknown as FakeTrack).readyState, 'live');
  conn.destroy();
});

test('setLocalStream replaces tracks and survives peer rebuild', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);

  const firstStream = asStream(new FakeStream([new FakeTrack({ kind: 'audio' })]));
  const secondStream = asStream(new FakeStream([new FakeTrack({ kind: 'video' })]));

  conn.setLocalStream(firstStream);
  assert.equal(conn.currentLocalStream, firstStream);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 1);

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-9' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.addTrackCalls.length, 1);
  assert.equal(peer.addTrackCalls[0]!.track, firstStream.getTracks()[0] as unknown as MediaStreamTrack);

  // Replace the local stream: the old track must be removed and the new one added.
  conn.setLocalStream(secondStream);
  assert.equal(conn.currentLocalStream, secondStream);
  // removeTrack called on the peer for the old track.
  assert.equal(peer.removeTrackCalls.length, 1);
  assert.equal(peer.removeTrackCalls[0]!.track, firstStream.getTracks()[0] as unknown as MediaStreamTrack);
  // addTrack called for the new track.
  assert.equal(peer.addTrackCalls.length, 2);
  assert.equal(peer.addTrackCalls[1]!.track, secondStream.getTracks()[0] as unknown as MediaStreamTrack);

  // After a rebuild, only the second stream's track should be re-attached.
  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-ep-9b' });
  await flush();
  const secondPeer = h.peers[1] as FakePeer;
  assert.equal(secondPeer.addTrackCalls.length, 1);
  assert.equal(secondPeer.addTrackCalls[0]!.track, secondStream.getTracks()[0] as unknown as MediaStreamTrack);

  // setLocalStream(null) removes everything.
  conn.setLocalStream(null);
  assert.equal(conn.currentLocalStream, null);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 0);
  conn.destroy();
});

test('localStream constructor option attaches tracks to the first peer', async () => {
  const h = createFakeHarness();
  const stream = asStream(new FakeStream([new FakeTrack({ kind: 'audio' }), new FakeTrack({ kind: 'video' })]));
  const conn = new PeerConnection({
    url: 'wss://signal.example/v1/signal',
    transportFactory: h.transportFactory,
    simplePeerFactory: h.simplePeerFactory,
    localStream: stream,
  });
  assert.equal(conn.currentLocalStream, stream);
  assert.equal(conn.mediaDiagnostics.desiredTrackCount, 2);

  const created = conn.createRoom();
  const createMsg = await waitForMessage(h, 'create-room');
  h.ws.receiveMessage(createRoomResponse('room-ls', 'tok-host', createMsg['hostEpoch'] as string));
  await created;
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-ls' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  assert.equal(peer.addTrackCalls.length, 2);
  conn.destroy();
});

test('peer-created and peer-destroyed events fire with monotonic generations', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const created: { peer: unknown; generation: number }[] = [];
  const destroyed: { generation: number; reason: string }[] = [];
  conn.on('peer-created', (e) => created.push(e));
  conn.on('peer-destroyed', (e) => destroyed.push(e));

  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-gen' });
  await flush();
  assert.equal(created.length, 1);
  assert.equal(created[0]!.generation, 1);
  assert.equal(destroyed.length, 0);

  h.ws.receiveMessage({ type: 'peer-reset', role: 'guest', epoch: 'guest-ep-gen2' });
  await flush();
  assert.equal(created.length, 2);
  assert.equal(created[1]!.generation, 2);
  assert.equal(destroyed.length, 1);
  assert.equal(destroyed[0]!.generation, 1);
  assert.equal(destroyed[0]!.reason, 'rebuild');

  conn.close('done');
  await flush();
  // The close-driven destroy fires with reason 'user-close'.
  const userClose = destroyed.find((d) => d.reason === 'user-close');
  assert.ok(userClose, 'expected a peer-destroyed with reason user-close');
  conn.destroy();
});

test('addTrack failure emits media-error and does not throw', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stream = asStream(new FakeStream());
  const track = asTrack(new FakeTrack({ kind: 'audio' }));
  conn.addTrack(track, stream);

  // Make the next peer's addTrack throw.
  // We do this by waiting for guest-joined and configuring the peer after build.
  // Instead, configure the factory to throw on the first addTrack call.
  // Simplest: build the peer, then add a track that fails.
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-err' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.addTrackThrows = new Error('boom');

  const newTrack = asTrack(new FakeTrack({ kind: 'video' }));
  const errors: { message: string }[] = [];
  conn.on('media-error', (e) => errors.push(e));
  // This must not throw; it should emit media-error instead.
  conn.addTrack(newTrack, stream);
  assert.equal(errors.length, 1);
  assert.match(errors[0]!.message, /addTrack failed/);
  conn.destroy();
});

test('on() returns an unsubscribe function (rec 7)', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const seen: string[] = [];
  const off = conn.on('stream', (s) => seen.push((s as unknown as { id: string }).id));

  assert.equal(typeof off, 'function');
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-unsub' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  const s1 = asStream(new FakeStream());
  peer.emitStream(s1);
  await flush();
  assert.equal(seen.length, 1);

  off();
  const s2 = asStream(new FakeStream());
  peer.emitStream(s2);
  await flush();
  assert.equal(seen.length, 1);
  conn.destroy();
});

test('rawPeer escape hatch returns the underlying peer or null', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  assert.equal(conn.rawPeer, null);
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-raw' });
  await flush();
  assert.ok(conn.rawPeer !== null);
  // After destroy, rawPeer becomes null again.
  conn.destroy();
  assert.equal(conn.rawPeer, null);
});

test('getStats passthrough returns null when no peer exists', async () => {
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  const stats = await conn.getStats();
  assert.equal(stats, null);
  conn.destroy();
});

test('renegotiation glare: simultaneous addTrack on both peers does not corrupt data frames', async () => {
  // This test exercises the wrapper's desired-media registry under a
  // renegotiation that happens while a data frame is in flight. The wrapper
  // must not interleave media attachment with data-channel framing.
  const h = createFakeHarness();
  const conn = await hostCreatesRoom(h);
  h.ws.receiveMessage({ type: 'guest-joined', guestEpoch: 'guest-ep-glare' });
  await flush();
  const peer = h.peers[0] as FakePeer;
  peer.emitConnect();
  await flush();

  // Release the socket so renegotiation goes over the data channel.
  h.ws.receiveMessage({ type: 'room-idle-close', reason: 'peer-connected' });
  await flush();
  h.ws.closeFromServer(CloseCode.RELEASED_AFTER_PEER_CONNECTED, 'released');
  await flush();

  const dataReceived: unknown[] = [];
  conn.on('data', (d) => dataReceived.push(d));

  // Interleave a data frame, a local addTrack (which triggers an outbound
  // renegotiate signal over the data channel), and an inbound renegotiate
  // frame from the remote peer. The wrapper must keep data frames and
  // renegotiation control frames separate.
  peer.emitData('hello-data');
  const stream = asStream(new FakeStream());
  conn.addTrack(asTrack(new FakeTrack({ kind: 'audio' })), stream);
  // Inbound renegotiate frame from the remote peer must be fed to peer.signal.
  const beforeSignals = peer.signals.length;
  const renegotiateFrame = JSON.stringify({
    kind: 'renegotiate',
    signal: { type: 'renegotiate', renegotiate: true },
  });
  peer.emitData(renegotiateFrame);
  peer.emitData('world-data');
  await flush();

  // The wrapper emits every chunk to `data` (including control frames, which
  // is existing behavior), but application data must still arrive intact and
  // in order. The renegotiate frame must additionally be forwarded to
  // peer.signal without corrupting the surrounding data frames.
  assert.deepEqual(dataReceived, ['hello-data', renegotiateFrame, 'world-data']);
  assert.equal(peer.signals.length, beforeSignals + 1);
  conn.destroy();
});
