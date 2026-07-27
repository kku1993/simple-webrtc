import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SIGNAL_VERSION, parseSignalData, signalFrame } from '../src/rtc/signal.js';

test('signalFrame stamps the envelope version', () => {
  assert.deepEqual(signalFrame({ t: 'renegotiate' }), { t: 'renegotiate', v: SIGNAL_VERSION });
});

test('parseSignalData round-trips every payload variant', () => {
  const frames = [
    signalFrame({ t: 'offer', sdp: 'v=0 offer' }),
    signalFrame({ t: 'answer', sdp: 'v=0 answer' }),
    signalFrame({ t: 'candidate', candidate: { candidate: 'a=cand', sdpMid: '0', sdpMLineIndex: 0 } }),
    signalFrame({ t: 'renegotiate' }),
    signalFrame({ t: 'transceiver', kind: 'video' }),
  ];
  for (const frame of frames) {
    const parsed = parseSignalData(JSON.parse(JSON.stringify(frame)));
    assert.deepEqual(parsed, frame, `round-trip failed for ${frame.t}`);
  }
});

test('parseSignalData rejects unknown and malformed frames without throwing', () => {
  const bad: unknown[] = [
    null,
    undefined,
    'offer',
    42,
    [],
    {},
    { t: 'unknown-future-variant' },
    { t: 'offer' }, // missing sdp
    { t: 'offer', sdp: 42 },
    { t: 'answer', sdp: null },
    { t: 'candidate' },
    { t: 'candidate', candidate: null },
    { t: 'candidate', candidate: {} }, // missing candidate string
    { t: 'transceiver' }, // missing kind
  ];
  for (const v of bad) {
    assert.equal(parseSignalData(v), null, `expected null for ${JSON.stringify(v)}`);
  }
});

test('parseSignalData rejects a future envelope version', () => {
  assert.equal(parseSignalData({ v: SIGNAL_VERSION + 1, t: 'renegotiate' }), null);
});

test('parseSignalData accepts a frame with no version (defaults to current)', () => {
  assert.deepEqual(parseSignalData({ t: 'renegotiate' }), {
    v: SIGNAL_VERSION,
    t: 'renegotiate',
  });
});

test('parseSignalData preserves an empty end-of-candidates string', () => {
  const parsed = parseSignalData({ t: 'candidate', candidate: { candidate: '' } });
  assert.deepEqual(parsed, { v: SIGNAL_VERSION, t: 'candidate', candidate: { candidate: '' } });
});

test('parseSignalData drops unexpected candidate fields', () => {
  const parsed = parseSignalData({
    t: 'candidate',
    candidate: { candidate: 'a=cand', address: '192.168.1.5', port: 5000 },
  });
  assert.deepEqual(parsed, {
    v: SIGNAL_VERSION,
    t: 'candidate',
    candidate: { candidate: 'a=cand' },
  });
});
