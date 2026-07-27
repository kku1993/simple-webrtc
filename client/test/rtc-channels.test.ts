import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RtcPeer } from '../src/rtc/peer.js';
import {
  CONTROL_CHANNEL_LABEL,
  DEFAULT_CHANNEL_LABEL,
  resolveSpec,
} from '../src/rtc/channels.js';
import { type DataChannelHandle } from '../src/rtc/channel-handle.js';
import { DataChannelNotOpenError } from '../src/errors.js';
import type { DataChannelSpec, RtcPeerOptions } from '../src/rtc/types.js';
import { FakeDataChannel, FakePeerConnection, fakeRtcImpl, linkChannels, tick } from './rtc-fakes.js';

interface Harness {
  peer: RtcPeer;
  pc: FakePeerConnection;
}

function makePeer(opts: Partial<RtcPeerOptions> = {}): Harness {
  FakePeerConnection.reset();
  const peer = new RtcPeer({ initiator: true, rtcImpl: fakeRtcImpl, ...opts });
  return { peer, pc: FakePeerConnection.instances[0]! };
}

function bringUp(h: Harness): void {
  for (const ch of h.pc.createdChannels) ch.open();
  h.pc.setConnectionState('connected');
}

// ---------------------------------------------------------------------------
// Declaration and allocation
// ---------------------------------------------------------------------------

test('the initiator creates the control, default, and declared channels', () => {
  const h = makePeer({ dataChannels: { chat: {}, cursor: { ordered: false } } });
  assert.deepEqual(
    h.pc.createdChannels.map((c) => c.label).sort(),
    [CONTROL_CHANNEL_LABEL, DEFAULT_CHANNEL_LABEL, 'chat', 'cursor'].sort(),
  );
});

test('the responder creates nothing and binds inbound channels by label', () => {
  const h = makePeer({ initiator: false, dataChannels: { chat: {} } });
  assert.equal(h.pc.createdChannels.length, 0, 'the initiator is the sole creator');

  h.pc.emitDataChannel(new FakeDataChannel('chat'));
  assert.equal(h.peer.channel('chat').readyState, 'connecting');

  const inbound = new FakeDataChannel('chat');
  h.pc.emitDataChannel(inbound);
  inbound.open();
  assert.equal(h.peer.channel('chat').readyState, 'open');
});

test('channel configuration is forwarded verbatim', () => {
  const h = makePeer({
    dataChannels: {
      cursor: { ordered: false, maxRetransmits: 0, protocol: 'cursor-v1' },
      telemetry: { ordered: false, maxPacketLifeTime: 100 },
    },
  });
  const cursor = h.pc.channel('cursor');
  assert.equal(cursor.ordered, false);
  assert.equal(cursor.maxRetransmits, 0);
  assert.equal(cursor.protocol, 'cursor-v1');

  const telemetry = h.pc.channel('telemetry');
  assert.equal(telemetry.maxPacketLifeTime, 100);
});

test('mutually exclusive reliability options are rejected', () => {
  assert.throws(
    () => resolveSpec({ maxRetransmits: 3, maxPacketLifeTime: 100 }),
    /mutually exclusive/,
  );
  assert.throws(
    () => makePeer({ dataChannels: { bad: { maxRetransmits: 3, maxPacketLifeTime: 100 } } }),
    /mutually exclusive/,
  );
});

test('reserved labels are rejected', () => {
  assert.throws(() => makePeer({ dataChannels: { __sps_evil: {} } }), /reserved/);
  const h = makePeer();
  assert.throws(() => h.peer.channel(CONTROL_CHANNEL_LABEL), /reserved/);
  assert.throws(() => h.peer.openChannel('__sps_x'), /reserved/);
});

test('an undeclared label throws with a pointer to openChannel', () => {
  const h = makePeer();
  assert.throws(() => h.peer.channel('nope'), /openChannel/);
});

test('reserved channels are hidden from the public channel map', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  assert.deepEqual([...h.peer.dataChannels.keys()], ['chat']);
  // Diagnostics still cover them, since that is where they matter.
  assert.equal(h.peer.channelDiagnostics.length, 3);
});

// ---------------------------------------------------------------------------
// Dynamic channels
// ---------------------------------------------------------------------------

test('openChannel creates a channel at runtime', () => {
  const h = makePeer();
  const handle = h.peer.openChannel('file-42', { ordered: true });
  assert.equal(h.pc.channel('file-42').label, 'file-42');
  assert.equal(h.peer.channel('file-42'), handle, 'same handle returned by channel()');
});

test('openChannel is idempotent for an already known label', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  const first = h.peer.channel('chat');
  const again = h.peer.openChannel('chat');
  assert.equal(again, first);
  assert.equal(h.pc.createdChannels.filter((c) => c.label === 'chat').length, 1);
});

test('a channel opened by the remote is adopted and announced', () => {
  const h = makePeer({ initiator: false });
  const announced: string[] = [];
  h.peer.on('channel', ({ label }) => announced.push(label));

  const inbound = new FakeDataChannel('surprise', { ordered: false, maxRetransmits: 0 });
  h.pc.emitDataChannel(inbound);

  assert.deepEqual(announced, ['surprise']);
  // The creating side is the authority on reliability, so the adopted spec is
  // read off the channel rather than guessed.
  assert.equal(h.peer.channel('surprise').ordered, false);
});

test('adopting a declared channel does not announce it as remote', () => {
  const h = makePeer({ initiator: false, dataChannels: { chat: {} } });
  const announced: string[] = [];
  h.peer.on('channel', ({ label }) => announced.push(label));

  h.pc.emitDataChannel(new FakeDataChannel('chat'));
  assert.deepEqual(announced, []);
});

// ---------------------------------------------------------------------------
// whenClosed policy
// ---------------------------------------------------------------------------

test('reliable channels buffer by default and unreliable ones throw', () => {
  assert.equal(resolveSpec({}).whenClosed, 'buffer');
  assert.equal(resolveSpec({ ordered: true }).whenClosed, 'buffer');
  // Flushing stale state after a reconnect is worse than dropping it.
  assert.equal(resolveSpec({ ordered: false, maxRetransmits: 0 }).whenClosed, 'throw');
  assert.equal(resolveSpec({ maxPacketLifeTime: 50 }).whenClosed, 'throw');
  assert.equal(resolveSpec({ ordered: false }).whenClosed, 'throw');
});

test('buffered messages flush in order when the channel opens', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  const chat = h.peer.channel('chat');
  chat.send('one');
  chat.send('two');
  assert.equal(chat.queuedCount, 2);
  assert.deepEqual(h.pc.channel('chat').sent, []);

  h.pc.channel('chat').open();
  assert.deepEqual(h.pc.channel('chat').sent, ['one', 'two']);
  assert.equal(chat.queuedCount, 0);
});

test('the send queue drops the oldest message at its limit', () => {
  const h = makePeer({ dataChannels: { chat: { bufferLimit: 2 } } });
  const chat = h.peer.channel('chat');
  const errors: string[] = [];
  chat.on('error', ({ message }) => errors.push(message));

  chat.send('a');
  chat.send('b');
  chat.send('c');
  assert.equal(chat.queuedCount, 2);
  assert.equal(errors.length, 1);
  assert.match(errors[0]!, /queue full/);

  h.pc.channel('chat').open();
  assert.deepEqual(h.pc.channel('chat').sent, ['b', 'c']);
});

test("the 'throw' policy raises DataChannelNotOpenError", () => {
  const h = makePeer({ dataChannels: { cursor: { ordered: false, maxRetransmits: 0 } } });
  const cursor = h.peer.channel('cursor');
  assert.throws(() => cursor.send('x'), DataChannelNotOpenError);
  try {
    cursor.send('x');
  } catch (e) {
    assert.equal((e as DataChannelNotOpenError).label, 'cursor');
    assert.equal((e as DataChannelNotOpenError).readyState, 'connecting');
  }
});

test("the 'drop' policy discards silently", () => {
  const h = makePeer({ dataChannels: { noisy: { whenClosed: 'drop' } } });
  const noisy = h.peer.channel('noisy');
  noisy.send('vanishes');
  assert.equal(noisy.queuedCount, 0);
  h.pc.channel('noisy').open();
  assert.deepEqual(h.pc.channel('noisy').sent, []);
});

// ---------------------------------------------------------------------------
// Routing and events
// ---------------------------------------------------------------------------

test('messages route to the right handle and to channel-message', () => {
  const h = makePeer({ dataChannels: { chat: {}, cursor: { ordered: false } } });
  bringUp(h);

  const perHandle: string[] = [];
  const aggregate: { label: string; data: unknown }[] = [];
  h.peer.channel('chat').on('message', (d) => perHandle.push(`chat:${String(d)}`));
  h.peer.channel('cursor').on('message', (d) => perHandle.push(`cursor:${String(d)}`));
  h.peer.on('channel-message', (m) => aggregate.push(m));

  h.pc.channel('chat').deliver('hello');
  h.pc.channel('cursor').deliver('10,20');

  assert.deepEqual(perHandle, ['chat:hello', 'cursor:10,20']);
  assert.deepEqual(aggregate, [
    { label: 'chat', data: 'hello' },
    { label: 'cursor', data: '10,20' },
  ]);
});

test('channel-open and channel-close fire for application channels only', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  const opened: string[] = [];
  const closed: string[] = [];
  h.peer.on('channel-open', ({ label }) => opened.push(label));
  h.peer.on('channel-close', ({ label }) => closed.push(label));

  bringUp(h);
  assert.deepEqual(opened, ['chat'], 'reserved channels stay internal');

  h.pc.channel('chat').close();
  assert.deepEqual(closed, ['chat']);
});

test('two linked channels deliver end to end', () => {
  const a = new FakeDataChannel('chat');
  const b = new FakeDataChannel('chat');
  linkChannels(a, b);
  a.open();
  b.open();

  const received: unknown[] = [];
  b.onmessage = (ev): void => {
    received.push(ev.data);
  };
  a.send('ping');
  assert.deepEqual(received, ['ping']);
});

// ---------------------------------------------------------------------------
// Handle lifecycle
// ---------------------------------------------------------------------------

test('a shared handle registry keeps identity across peer generations', () => {
  const handles = new Map<string, DataChannelHandle>();
  FakePeerConnection.reset();

  const first = new RtcPeer({
    initiator: true,
    rtcImpl: fakeRtcImpl,
    dataChannels: { chat: {} },
    channelHandles: handles,
  });
  const handle = first.channel('chat');
  const messages: unknown[] = [];
  // An application listener registered once, before any rebuild.
  handle.on('message', (d) => messages.push(d));

  const pc1 = FakePeerConnection.instances[0]!;
  pc1.channel('chat').open();
  pc1.channel('chat').deliver('gen-1');
  first.destroy();

  // Epoch changed: the peer is thrown away and rebuilt.
  const second = new RtcPeer({
    initiator: true,
    rtcImpl: fakeRtcImpl,
    dataChannels: { chat: {} },
    channelHandles: handles,
  });
  assert.equal(second.channel('chat'), handle, 'identity survives the rebuild');

  const pc2 = FakePeerConnection.instances[1]!;
  pc2.channel('chat').open();
  pc2.channel('chat').deliver('gen-2');

  assert.deepEqual(messages, ['gen-1', 'gen-2'], 'the listener survives too');
});

test('a destroyed generation stops emitting on a shared handle', () => {
  const handles = new Map<string, DataChannelHandle>();
  FakePeerConnection.reset();

  const first = new RtcPeer({
    initiator: true,
    rtcImpl: fakeRtcImpl,
    dataChannels: { chat: {} },
    channelHandles: handles,
  });
  const seen: string[] = [];
  first.on('channel-message', ({ data }) => seen.push(`first:${String(data)}`));

  first.destroy();

  const second = new RtcPeer({
    initiator: true,
    rtcImpl: fakeRtcImpl,
    dataChannels: { chat: {} },
    channelHandles: handles,
  });
  second.on('channel-message', ({ data }) => seen.push(`second:${String(data)}`));

  const pc2 = FakePeerConnection.instances[1]!;
  pc2.channel('chat').open();
  pc2.channel('chat').deliver('x');

  // The superseded generation must not still be listening on the shared handle.
  assert.deepEqual(seen, ['second:x']);
});

test('destroy unbinds handles but a shared registry stays usable', () => {
  const handles = new Map<string, DataChannelHandle>();
  FakePeerConnection.reset();
  const peer = new RtcPeer({
    initiator: true,
    rtcImpl: fakeRtcImpl,
    dataChannels: { chat: {} },
    channelHandles: handles,
  });
  const handle = peer.channel('chat');
  FakePeerConnection.instances[0]!.channel('chat').open();

  peer.destroy();
  assert.equal(handle.readyState, 'connecting', 'unbound, not retired');
  // Still buffers for the next generation rather than throwing.
  handle.send('queued-for-next-gen');
  assert.equal(handle.queuedCount, 1);
});

test('without a shared registry, destroy retires the handles', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  const handle = h.peer.channel('chat');
  h.pc.channel('chat').open();

  h.peer.destroy();
  assert.equal(handle.readyState, 'closed');
});

test('a handle reports close when its generation is torn down', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  const handle = h.peer.channel('chat');
  let closes = 0;
  handle.on('close', () => closes++);

  h.pc.channel('chat').open();
  h.peer.destroy();
  assert.equal(closes, 1);
});

// ---------------------------------------------------------------------------
// Backpressure and failures
// ---------------------------------------------------------------------------

test('drain fires once after buffered data clears', () => {
  const h = makePeer({ dataChannels: { bulk: {} } });
  bringUp(h);
  const bulk = h.peer.channel('bulk');
  let drains = 0;
  bulk.on('drain', () => drains++);

  const raw = h.pc.channel('bulk');
  raw.bufferedAmount = 1024 * 1024;
  bulk.send('big');
  assert.equal(bulk.bufferedAmount, 1024 * 1024);

  raw.bufferedAmount = 0;
  raw.fireDrain();
  assert.equal(drains, 1);

  // No pending backpressure, so a stray event does not re-fire.
  raw.fireDrain();
  assert.equal(drains, 1);
});

test('a send failure surfaces as a channel error, not a throw', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  bringUp(h);
  const errors: { label: string; message: string }[] = [];
  h.peer.on('channel-error', (e) => errors.push(e));

  h.pc.channel('chat').sendThrows = new Error('OperationError');
  h.peer.channel('chat').send('boom');

  assert.equal(errors.length, 1);
  assert.equal(errors[0]!.label, 'chat');
  assert.equal(h.peer.destroyed, false);
});

test('a channel error event is reported without affecting the connection', () => {
  const h = makePeer({ dataChannels: { chat: {} } });
  bringUp(h);
  const errors: { label: string; message: string }[] = [];
  h.peer.on('channel-error', (e) => errors.push(e));

  h.pc.channel('chat').fireError(new Error('sctp failure'));
  assert.deepEqual(errors.map((e) => e.message), ['sctp failure']);
  assert.equal(h.peer.connected, true);
});

test('a channel that is already open when adopted still reports open', () => {
  const h = makePeer({ initiator: false, dataChannels: { chat: {} } });
  let opened = 0;
  h.peer.on('channel-open', () => opened++);

  const inbound = new FakeDataChannel('chat');
  inbound.readyState = 'open';
  h.pc.emitDataChannel(inbound);

  // `onopen` never fires for a channel handed over already open.
  assert.equal(opened, 1);
});

test('declared channels the peer never created are reported at connect', async () => {
  const logged: { msg: string; ctx?: Record<string, unknown> }[] = [];
  FakePeerConnection.reset();
  new RtcPeer({
    initiator: false,
    rtcImpl: fakeRtcImpl,
    dataChannels: { chat: {}, orphan: {} },
    logger: { warn: (msg, ctx) => logged.push({ msg, ...(ctx ? { ctx } : {}) }) },
  });
  const pc = FakePeerConnection.instances[0]!;

  for (const label of [CONTROL_CHANNEL_LABEL, DEFAULT_CHANNEL_LABEL, 'chat']) {
    const ch = new FakeDataChannel(label);
    pc.emitDataChannel(ch);
    ch.open();
  }
  pc.setConnectionState('connected');
  await tick();

  const warning = logged.find((l) => l.msg.includes('never opened'));
  assert.ok(warning, 'a one-sided declaration should be surfaced');
  assert.equal(warning.ctx?.labels, 'orphan');
});

test('diagnostics expose per-channel state without message contents', () => {
  const h = makePeer({ dataChannels: { chat: {}, cursor: { ordered: false } } });
  bringUp(h);
  h.peer.channel('chat').send('secret');

  const chat = h.peer.channelDiagnostics.find((d) => d.label === 'chat')!;
  assert.equal(chat.readyState, 'open');
  assert.equal(chat.ordered, true);
  assert.equal(chat.queued, 0);
  assert.equal(
    JSON.stringify(h.peer.channelDiagnostics).includes('secret'),
    false,
    'diagnostics must not carry payloads',
  );

  const cursor = h.peer.channelDiagnostics.find((d) => d.label === 'cursor')!;
  assert.equal(cursor.ordered, false);
});

test('resolveSpec preserves explicit overrides', () => {
  const spec: DataChannelSpec = { ordered: false, whenClosed: 'buffer', bufferLimit: 5 };
  const resolved = resolveSpec(spec);
  assert.equal(resolved.whenClosed, 'buffer');
  assert.equal(resolved.bufferLimit, 5);
  assert.equal(resolved.ordered, false);
});
