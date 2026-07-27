import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RtcPeer } from '../src/rtc/peer.js';
import { CONTROL_CHANNEL_LABEL, DEFAULT_CHANNEL_LABEL } from '../src/rtc/channels.js';
import { type SignalData, signalFrame } from '../src/rtc/signal.js';
import type { RtcPeerOptions } from '../src/rtc/types.js';
import { FakePeerConnection, fakeRtcImpl, tick } from './rtc-fakes.js';

interface Harness {
  peer: RtcPeer;
  pc: FakePeerConnection;
  signals: SignalData[];
  errors: Error[];
  closes: number;
}

function makePeer(opts: Partial<RtcPeerOptions> = {}): Harness {
  FakePeerConnection.reset();
  const peer = new RtcPeer({ initiator: true, rtcImpl: fakeRtcImpl, ...opts });
  const h: Harness = {
    peer,
    pc: FakePeerConnection.instances[0]!,
    signals: [],
    errors: [],
    closes: 0,
  };
  peer.on('signal', (s) => h.signals.push(s));
  peer.on('error', (e) => h.errors.push(e));
  peer.on('close', () => h.closes++);
  return h;
}

function signalsOfType(h: Harness, t: SignalData['t']): SignalData[] {
  return h.signals.filter((s) => s.t === t);
}

/** Bring a peer to `connect`: control channel open plus a connected transport. */
function bringUp(h: Harness): void {
  h.pc.channel(CONTROL_CHANNEL_LABEL).open();
  h.pc.channel(DEFAULT_CHANNEL_LABEL).open();
  h.pc.setConnectionState('connected');
}

const track = (id = 't1'): MediaStreamTrack =>
  ({ id, kind: 'audio', readyState: 'live' }) as unknown as MediaStreamTrack;
const stream = (id = 's1'): MediaStream =>
  ({ id, getTracks: () => [] }) as unknown as MediaStream;

// ---------------------------------------------------------------------------
// Negotiation
// ---------------------------------------------------------------------------

test('initiator offers on construction; responder does not', async () => {
  const host = makePeer({ initiator: true });
  const guest = makePeer({ initiator: false });
  await tick();

  assert.equal(signalsOfType(host, 'offer').length, 1);
  // The responder discards its own initial negotiation request: the initiator
  // is about to offer anyway, and racing it would be pointless churn.
  assert.equal(guest.signals.length, 0);
});

test('mutations in the same tick collapse into a single offer', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  assert.equal(signalsOfType(h, 'offer').length, 1);

  // Two synchronous mutations must produce one renegotiation, not two.
  h.peer.addTrack(track('a'), stream());
  h.peer.addTrack(track('b'), stream());
  h.pc.setSignalingState('stable');
  await tick();

  assert.equal(signalsOfType(h, 'offer').length, 2);
});

test('a renegotiation requested mid-exchange runs exactly once on stable', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  assert.equal(h.pc.signalingState, 'have-local-offer');

  // Three requests while the first exchange is still in flight.
  h.peer.addTrack(track('a'), stream());
  await tick();
  h.peer.addTrack(track('b'), stream());
  await tick();
  assert.equal(signalsOfType(h, 'offer').length, 1, 'must not offer while unstable');

  h.pc.setSignalingState('stable');
  await tick();
  assert.equal(signalsOfType(h, 'offer').length, 2, 'exactly one queued renegotiation');
});

test('responder asks the initiator to renegotiate instead of offering', async () => {
  const h = makePeer({ initiator: false });
  await tick();
  h.peer.addTrack(track(), stream());
  await tick();

  assert.equal(signalsOfType(h, 'offer').length, 0);
  assert.equal(signalsOfType(h, 'renegotiate').length, 1);
});

test('initiator honors a renegotiate request; responder ignores one', async () => {
  const host = makePeer({ initiator: true });
  await tick();
  host.pc.setSignalingState('stable');
  host.peer.signal(signalFrame({ t: 'renegotiate' }));
  await tick();
  assert.equal(signalsOfType(host, 'offer').length, 2);

  const guest = makePeer({ initiator: false });
  await tick();
  guest.peer.signal(signalFrame({ t: 'renegotiate' }));
  await tick();
  assert.equal(guest.signals.length, 0);
});

test('a remote offer produces an answer; a remote answer produces nothing', async () => {
  const guest = makePeer({ initiator: false });
  await tick();
  guest.peer.signal(signalFrame({ t: 'offer', sdp: 'remote-offer' }));
  await tick();

  assert.equal(guest.pc.remoteDescription?.sdp, 'remote-offer');
  assert.equal(signalsOfType(guest, 'answer').length, 1);

  const host = makePeer({ initiator: true });
  await tick();
  const before = host.signals.length;
  host.peer.signal(signalFrame({ t: 'answer', sdp: 'remote-answer' }));
  await tick();
  assert.equal(host.signals.length, before, 'an answer needs no reply');
});

// ---------------------------------------------------------------------------
// ICE
// ---------------------------------------------------------------------------

test('candidates arriving before the remote description are queued, then flushed', async () => {
  const h = makePeer({ initiator: true });
  await tick();

  h.peer.signal(signalFrame({ t: 'candidate', candidate: { candidate: 'c1' } }));
  h.peer.signal(signalFrame({ t: 'candidate', candidate: { candidate: 'c2' } }));
  await tick();
  assert.equal(h.pc.addedCandidates.length, 0, 'no remote description yet');

  h.peer.signal(signalFrame({ t: 'answer', sdp: 'a' }));
  await tick();

  assert.deepEqual(
    h.pc.addedCandidates.map((c) => c.candidate),
    ['c1', 'c2'],
    'queued candidates flush in order',
  );
});

test('candidates arriving after the remote description apply immediately', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.peer.signal(signalFrame({ t: 'answer', sdp: 'a' }));
  await tick();

  h.peer.signal(signalFrame({ t: 'candidate', candidate: { candidate: 'c3' } }));
  await tick();
  assert.deepEqual(
    h.pc.addedCandidates.map((c) => c.candidate),
    ['c3'],
  );
});

test('an unusable ICE candidate is not fatal', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.peer.signal(signalFrame({ t: 'answer', sdp: 'a' }));
  await tick();

  h.pc.addIceCandidateRejects = new Error('OperationError: unsupported candidate');
  h.peer.signal(signalFrame({ t: 'candidate', candidate: { candidate: 'bad.local' } }));
  await tick();

  // ICE fails on its own if no pair works; one rejected candidate must never
  // tear down a connection that might still succeed.
  assert.equal(h.closes, 0);
  assert.equal(h.errors.length, 0);
  assert.equal(h.peer.destroyed, false);
});

test('local ICE candidates are emitted as signal frames', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.pc.emitIceCandidate({
    candidate: 'candidate:1 1 udp',
    sdpMid: '0',
    sdpMLineIndex: 0,
    usernameFragment: 'ufrag',
  });

  const emitted = signalsOfType(h, 'candidate');
  assert.equal(emitted.length, 1);
  assert.deepEqual(emitted[0], {
    v: 1,
    t: 'candidate',
    candidate: {
      candidate: 'candidate:1 1 udp',
      sdpMid: '0',
      sdpMLineIndex: 0,
      usernameFragment: 'ufrag',
    },
  });
});

test('a null candidate (end of gathering) emits nothing', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  const before = h.signals.length;
  h.pc.emitIceCandidate(null);
  assert.equal(h.signals.length, before);
});

// ---------------------------------------------------------------------------
// Connect / teardown
// ---------------------------------------------------------------------------

test('connect requires both a connected transport and an open control channel', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  let connected = 0;
  h.peer.on('connect', () => connected++);

  h.pc.setConnectionState('connected');
  assert.equal(connected, 0, 'transport alone is not enough');

  h.pc.channel(CONTROL_CHANNEL_LABEL).open();
  assert.equal(connected, 1);
  assert.equal(h.peer.connected, true);

  // Idempotent: further transitions do not re-fire.
  h.pc.setIceConnectionState('completed');
  assert.equal(connected, 1);
});

test('control channel opening before the transport also connects', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  let connected = 0;
  h.peer.on('connect', () => connected++);

  h.pc.channel(CONTROL_CHANNEL_LABEL).open();
  assert.equal(connected, 0, 'channel alone is not enough');
  h.pc.setIceConnectionState('connected');
  assert.equal(connected, 1);
});

test('destroy is idempotent and closes the underlying connection', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.peer.destroy();
  h.peer.destroy();
  h.peer.destroy(new Error('late'));

  assert.equal(h.closes, 1);
  assert.equal(h.errors.length, 0);
  assert.equal(h.pc.closed, true);
  assert.equal(h.peer.destroyed, true);
});

test('a failed connection emits error then close', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  const order: string[] = [];
  h.peer.on('error', () => order.push('error'));
  h.peer.on('close', () => order.push('close'));

  h.pc.setConnectionState('failed');
  assert.deepEqual(order, ['error', 'close']);
});

test('failed ICE tears the peer down', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.pc.setIceConnectionState('failed');
  assert.equal(h.peer.destroyed, true);
  assert.equal(h.closes, 1);
});

test('signals after destroy are ignored', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.peer.destroy();
  h.peer.signal(signalFrame({ t: 'offer', sdp: 'late' }));
  await tick();
  assert.equal(h.pc.remoteDescription, null);
});

test('an unrecognized signal frame is ignored, not fatal', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  h.peer.signal({ t: 'some-future-thing', payload: 1 });
  h.peer.signal(null);
  h.peer.signal('garbage');
  await tick();

  // A peer running a newer client must not be able to kill this one.
  assert.equal(h.peer.destroyed, false);
  assert.equal(h.closes, 0);
});

test('a failure to create an offer is fatal', async () => {
  FakePeerConnection.reset();
  const peer = new RtcPeer({ initiator: true, rtcImpl: fakeRtcImpl });
  const pc = FakePeerConnection.instances[0]!;
  pc.createOfferRejects = new Error('createOffer blew up');
  const errors: Error[] = [];
  peer.on('error', (e) => errors.push(e));
  await tick();

  assert.equal(errors.length, 1);
  assert.match(errors[0]!.message, /Failed to create offer/);
  assert.equal(peer.destroyed, true);
});

// ---------------------------------------------------------------------------
// Control channel
// ---------------------------------------------------------------------------

test('control frames are applied as signals and never surface as data', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  bringUp(h);

  const data: unknown[] = [];
  const channelMessages: unknown[] = [];
  h.peer.on('data', (d) => data.push(d));
  h.peer.on('channel-message', (m) => channelMessages.push(m));

  h.pc.setSignalingState('stable');
  h.pc.channel(CONTROL_CHANNEL_LABEL).deliver(
    JSON.stringify(signalFrame({ t: 'renegotiate' })),
  );
  await tick();

  // Applied as a signal...
  assert.equal(signalsOfType(h, 'offer').length, 2);
  // ...and invisible to the application.
  assert.equal(data.length, 0);
  assert.equal(channelMessages.length, 0);
});

test('sendControlSignal writes to the control channel only when open', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  const frame = signalFrame({ t: 'renegotiate' });

  assert.equal(h.peer.sendControlSignal(frame), false, 'closed channel refuses');

  bringUp(h);
  assert.equal(h.peer.sendControlSignal(frame), true);
  assert.deepEqual(h.pc.channel(CONTROL_CHANNEL_LABEL).sent, [JSON.stringify(frame)]);
});

test('application data flows on the default channel, not the control channel', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  bringUp(h);

  const data: unknown[] = [];
  h.peer.on('data', (d) => data.push(d));

  h.peer.send('hello');
  assert.deepEqual(h.pc.channel(DEFAULT_CHANNEL_LABEL).sent, ['hello']);
  assert.deepEqual(h.pc.channel(CONTROL_CHANNEL_LABEL).sent, []);

  h.pc.channel(DEFAULT_CHANNEL_LABEL).deliver('inbound');
  assert.deepEqual(data, ['inbound']);
});

test('an application JSON payload shaped like a control frame stays application data', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  bringUp(h);
  h.pc.setSignalingState('stable');

  const data: unknown[] = [];
  h.peer.on('data', (d) => data.push(d));

  // Under a shared channel this would have been sniffed as protocol traffic.
  const lookalike = JSON.stringify({ t: 'renegotiate', v: 1 });
  h.pc.channel(DEFAULT_CHANNEL_LABEL).deliver(lookalike);
  await tick();

  assert.deepEqual(data, [lookalike]);
  assert.equal(signalsOfType(h, 'offer').length, 1, 'no renegotiation was triggered');
});

// ---------------------------------------------------------------------------
// Transceivers
// ---------------------------------------------------------------------------

test('the responder delegates addTransceiver to the initiator', async () => {
  const guest = makePeer({ initiator: false });
  await tick();
  guest.peer.addTransceiver('video');

  assert.equal(guest.pc.transceivers.length, 0);
  assert.deepEqual(signalsOfType(guest, 'transceiver'), [
    { v: 1, t: 'transceiver', kind: 'video' },
  ]);
});

test('the initiator adds a transceiver on request and renegotiates', async () => {
  const host = makePeer({ initiator: true });
  await tick();
  host.pc.setSignalingState('stable');
  host.peer.signal(signalFrame({ t: 'transceiver', kind: 'video' }));
  await tick();

  assert.deepEqual(host.pc.transceivers, [{ kind: 'video' }]);
  assert.equal(signalsOfType(host, 'offer').length, 2);
});

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

test('getStats returns the native report untransformed', async () => {
  const h = makePeer({ initiator: true });
  await tick();
  const stats = await h.peer.getStats();
  assert.ok(stats instanceof Map, 'no flattening, no legacy array shape');
});

test('config is forwarded to the RTCPeerConnection', () => {
  const config: RTCConfiguration = { iceServers: [{ urls: 'stun:example:3478' }] };
  const h = makePeer({ initiator: true, config });
  assert.deepEqual(h.pc.config, config);
});

test('sdpTransform rewrites the offer that is signaled', async () => {
  const h = makePeer({
    initiator: true,
    sdpTransform: (sdp) => `${sdp}\r\na=custom`,
  });
  await tick();
  const offer = signalsOfType(h, 'offer')[0]!;
  assert.equal(offer.t === 'offer' && offer.sdp.endsWith('a=custom'), true);
});

test('a missing RTCPeerConnection names the escape hatch', () => {
  assert.throws(
    () => new RtcPeer({ initiator: true, rtcImpl: { RTCPeerConnection: undefined } }),
    /rtcImpl/,
  );
});
