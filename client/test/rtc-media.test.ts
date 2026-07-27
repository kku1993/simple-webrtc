import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RtcPeer } from '../src/rtc/peer.js';
import type { SignalData } from '../src/rtc/signal.js';
import { FakePeerConnection, fakeRtcImpl, tick } from './rtc-fakes.js';

interface Harness {
  peer: RtcPeer;
  pc: FakePeerConnection;
  signals: SignalData[];
}

function makePeer(initiator = true, streams?: MediaStream[]): Harness {
  FakePeerConnection.reset();
  const peer = new RtcPeer({
    initiator,
    rtcImpl: fakeRtcImpl,
    ...(streams ? { streams } : {}),
  });
  const h: Harness = { peer, pc: FakePeerConnection.instances[0]!, signals: [] };
  peer.on('signal', (s) => h.signals.push(s));
  return h;
}

function offerCount(h: Harness): number {
  return h.signals.filter((s) => s.t === 'offer').length;
}

class FakeTrack {
  constructor(
    readonly id: string,
    readonly kind = 'audio',
  ) {}
}

class FakeStream {
  constructor(
    readonly id: string,
    private readonly tracks: FakeTrack[] = [],
  ) {}
  getTracks(): FakeTrack[] {
    return [...this.tracks];
  }
}

const asTrack = (t: FakeTrack): MediaStreamTrack => t as unknown as MediaStreamTrack;
const asStream = (s: FakeStream): MediaStream => s as unknown as MediaStream;

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

test('addTrack registers a sender and triggers negotiation', async () => {
  const h = makePeer(true);
  await tick();
  h.pc.setSignalingState('stable');

  const t = new FakeTrack('a');
  const s = new FakeStream('s1', [t]);
  h.peer.addTrack(asTrack(t), asStream(s));
  await tick();

  assert.equal(h.pc.addedTracks.length, 1);
  assert.equal(h.pc.addedTracks[0]!.track, asTrack(t));
  assert.equal(offerCount(h), 2);
});

test('adding the same track to the same stream twice throws', async () => {
  const h = makePeer(true);
  await tick();
  const t = new FakeTrack('a');
  const s = new FakeStream('s1', [t]);
  h.peer.addTrack(asTrack(t), asStream(s));

  assert.throws(() => h.peer.addTrack(asTrack(t), asStream(s)), /already been added/);
});

test('the same track may be sent as part of two different streams', async () => {
  const h = makePeer(true);
  await tick();
  const t = new FakeTrack('a');
  const s1 = new FakeStream('s1', [t]);
  const s2 = new FakeStream('s2', [t]);

  // Senders are keyed by the (track, stream) pair, not by track alone.
  h.peer.addTrack(asTrack(t), asStream(s1));
  h.peer.addTrack(asTrack(t), asStream(s2));
  assert.equal(h.pc.addedTracks.length, 2);
});

test('re-adding a removed track is refused rather than silently dead', async () => {
  const h = makePeer(true);
  await tick();
  const t = new FakeTrack('a');
  const s = new FakeStream('s1', [t]);
  h.peer.addTrack(asTrack(t), asStream(s));
  h.peer.removeTrack(asTrack(t), asStream(s));

  // The transceiver is inactive; re-adding would produce a sender that never
  // transmits. Toggling `track.enabled` is the supported path.
  assert.throws(() => h.peer.addTrack(asTrack(t), asStream(s)), /has been removed/);
});

test('removeTrack on an unknown track throws', async () => {
  const h = makePeer(true);
  await tick();
  const t = new FakeTrack('ghost');
  const s = new FakeStream('s1', [t]);
  assert.throws(() => h.peer.removeTrack(asTrack(t), asStream(s)), /never added/);
});

test('a removal rejected mid-exchange is retried when signaling goes stable', async () => {
  const h = makePeer(true);
  await tick();
  const t = new FakeTrack('a');
  const s = new FakeStream('s1', [t]);
  h.peer.addTrack(asTrack(t), asStream(s));

  h.pc.removeTrackThrows = new Error('InvalidStateError');
  h.peer.removeTrack(asTrack(t), asStream(s));
  assert.equal(h.pc.removedSenders.length, 0, 'deferred, not failed');

  h.pc.removeTrackThrows = null;
  h.pc.setSignalingState('stable');
  await tick();
  assert.equal(h.pc.removedSenders.length, 1, 'retried on stable');
});

test('replaceTrack swaps the track without renegotiating', async () => {
  const h = makePeer(true);
  await tick();
  h.pc.setSignalingState('stable');
  await tick();
  const before = offerCount(h);

  const oldT = new FakeTrack('old');
  const newT = new FakeTrack('new');
  const s = new FakeStream('s1', [oldT]);
  h.peer.addTrack(asTrack(oldT), asStream(s));
  h.pc.setSignalingState('stable');
  await tick();
  const afterAdd = offerCount(h);
  assert.ok(afterAdd > before, 'addTrack renegotiates');

  await h.peer.replaceTrack(asTrack(oldT), asTrack(newT), asStream(s));
  h.pc.setSignalingState('stable');
  await tick();

  // Not renegotiating is the entire point of replaceTrack.
  assert.equal(offerCount(h), afterAdd);
});

test('a replaced track can subsequently be removed', async () => {
  const h = makePeer(true);
  await tick();
  const oldT = new FakeTrack('old');
  const newT = new FakeTrack('new');
  const s = new FakeStream('s1', [oldT]);
  h.peer.addTrack(asTrack(oldT), asStream(s));
  await h.peer.replaceTrack(asTrack(oldT), asTrack(newT), asStream(s));

  // The sender must have been re-keyed under the new track.
  h.peer.removeTrack(asTrack(newT), asStream(s));
  assert.equal(h.pc.removedSenders.length, 1);
});

test('replaceTrack on an unknown track rejects', async () => {
  const h = makePeer(true);
  await tick();
  const a = new FakeTrack('a');
  const b = new FakeTrack('b');
  const s = new FakeStream('s1', []);
  await assert.rejects(
    () => h.peer.replaceTrack(asTrack(a), asTrack(b), asStream(s)),
    /never added/,
  );
});

test('addStream and removeStream cover every track', async () => {
  const h = makePeer(true);
  await tick();
  const t1 = new FakeTrack('a');
  const t2 = new FakeTrack('b', 'video');
  const s = new FakeStream('s1', [t1, t2]);

  h.peer.addStream(asStream(s));
  assert.equal(h.pc.addedTracks.length, 2);

  h.peer.removeStream(asStream(s));
  assert.equal(h.pc.removedSenders.length, 2);
});

test('constructor streams are attached before the initial offer', async () => {
  const t = new FakeTrack('a');
  const s = new FakeStream('s1', [t]);
  const h = makePeer(true, [asStream(s)]);

  // Attached synchronously in the constructor, so the first offer includes them.
  assert.equal(h.pc.addedTracks.length, 1);
  await tick();
  assert.equal(offerCount(h), 1);
});

test('media calls after destroy are no-ops', async () => {
  const h = makePeer(true);
  await tick();
  h.peer.destroy();
  const t = new FakeTrack('a');
  const s = new FakeStream('s1', [t]);

  h.peer.addTrack(asTrack(t), asStream(s));
  h.peer.removeTrack(asTrack(t), asStream(s));
  h.peer.addStream(asStream(s));
  h.peer.removeStream(asStream(s));
  await h.peer.replaceTrack(asTrack(t), asTrack(t), asStream(s));

  assert.equal(h.pc.addedTracks.length, 0);
});

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

test('one stream event per stream id, however many tracks it carries', async () => {
  const h = makePeer(true);
  await tick();
  const streams: MediaStream[] = [];
  const tracks: MediaStreamTrack[] = [];
  h.peer.on('stream', (s) => streams.push(s));
  h.peer.on('track', ({ track }) => tracks.push(track));

  const remote = asStream(new FakeStream('remote-1'));
  h.pc.emitTrack(asTrack(new FakeTrack('audio')), [remote]);
  h.pc.emitTrack(asTrack(new FakeTrack('video', 'video')), [remote]);
  await tick();

  assert.equal(tracks.length, 2, 'track fires per track');
  assert.equal(streams.length, 1, 'stream fires once per stream id');
});

test('the stream event is deferred so every track is attached first', async () => {
  const h = makePeer(true);
  await tick();

  let tracksAtStreamTime = -1;
  let seen = 0;
  h.peer.on('track', () => seen++);
  h.peer.on('stream', () => {
    tracksAtStreamTime = seen;
  });

  const remote = asStream(new FakeStream('remote-1'));
  // Both tracks arrive in the same turn, as they do for a real getUserMedia
  // stream. A synchronous `stream` emit would hand out a half-filled stream.
  h.pc.emitTrack(asTrack(new FakeTrack('audio')), [remote]);
  h.pc.emitTrack(asTrack(new FakeTrack('video', 'video')), [remote]);
  await tick();

  assert.equal(tracksAtStreamTime, 2);
});

test('distinct remote streams each fire once', async () => {
  const h = makePeer(true);
  await tick();
  const streams: MediaStream[] = [];
  h.peer.on('stream', (s) => streams.push(s));

  h.pc.emitTrack(asTrack(new FakeTrack('a')), [asStream(new FakeStream('r1'))]);
  h.pc.emitTrack(asTrack(new FakeTrack('b')), [asStream(new FakeStream('r2'))]);
  await tick();

  assert.deepEqual(
    streams.map((s) => s.id),
    ['r1', 'r2'],
  );
});

test('a track belonging to two streams fires stream for each', async () => {
  const h = makePeer(true);
  await tick();
  const streams: MediaStream[] = [];
  h.peer.on('stream', (s) => streams.push(s));

  const t = asTrack(new FakeTrack('shared'));
  h.pc.emitTrack(t, [asStream(new FakeStream('r1')), asStream(new FakeStream('r2'))]);
  await tick();

  assert.equal(streams.length, 2);
});

test('inbound tracks after destroy are ignored', async () => {
  const h = makePeer(true);
  await tick();
  let seen = 0;
  h.peer.on('track', () => seen++);
  h.peer.destroy();

  h.pc.emitTrack(asTrack(new FakeTrack('a')), [asStream(new FakeStream('r1'))]);
  await tick();
  assert.equal(seen, 0);
});
