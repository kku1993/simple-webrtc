import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  generateEpoch,
  generateRequestId,
  base64url,
  randomBytes,
  fullJitterBackoff,
  SequenceCounter,
  isPlainObject,
  MAX_EPOCH_CHARS,
} from '../src/util.js';

test('generateEpoch returns base64url of 16 bytes (22 chars, no padding)', () => {
  const e = generateEpoch();
  assert.equal(e.length, 22);
  assert.ok(/^[A-Za-z0-9_-]+$/.test(e), `unexpected chars in epoch: ${e}`);
});

test('generateEpoch produces distinct values', () => {
  const a = generateEpoch();
  const b = generateEpoch();
  assert.notEqual(a, b);
});

test('generateEpoch respects byteLength', () => {
  assert.equal(generateEpoch(8).length, 11); // ceil(8*4/3) without padding
});

test('epochs stay within the protocol char cap', () => {
  for (let i = 0; i < 100; i++) {
    assert.ok(generateEpoch().length <= MAX_EPOCH_CHARS);
  }
});

test('base64url has no +, /, or =', () => {
  for (let i = 0; i < 50; i++) {
    const b = randomBytes(32);
    const s = base64url(b);
    assert.ok(!/[+/=]/.test(s), `unexpected char in ${s}`);
  }
});

test('generateRequestId is base64url and non-empty', () => {
  const id = generateRequestId();
  assert.ok(id.length > 0);
  assert.ok(/^[A-Za-z0-9_-]+$/.test(id));
});

test('fullJitterBackoff stays within [0, min(base*2^attempt, cap)]', () => {
  for (let attempt = 0; attempt < 10; attempt++) {
    const d = fullJitterBackoff(attempt, 500, 30_000);
    const upper = Math.min(500 * 2 ** attempt, 30_000);
    assert.ok(d >= 0 && d < upper, `attempt ${attempt}: ${d} not in [0, ${upper})`);
  }
});

test('fullJitterBackoff caps at capMs', () => {
  for (let i = 0; i < 100; i++) {
    assert.ok(fullJitterBackoff(20, 500, 30_000) < 30_000);
  }
});

test('SequenceCounter starts at 1 and increments', () => {
  const s = new SequenceCounter();
  assert.equal(s.next(), 1);
  assert.equal(s.next(), 2);
  assert.equal(s.next(), 3);
  assert.equal(s.current, 3);
});

test('SequenceCounter.reset returns to 0 so next is 1 again', () => {
  const s = new SequenceCounter();
  s.next();
  s.next();
  s.reset();
  assert.equal(s.current, 0);
  assert.equal(s.next(), 1);
});

test('isPlainObject', () => {
  assert.ok(isPlainObject({}));
  assert.ok(isPlainObject({ a: 1 }));
  assert.ok(!isPlainObject(null));
  assert.ok(!isPlainObject(undefined));
  assert.ok(!isPlainObject([]));
  assert.ok(!isPlainObject('x'));
  assert.ok(!isPlainObject(42));
});
