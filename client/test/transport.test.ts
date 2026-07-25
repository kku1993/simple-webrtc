import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseFrame } from '../src/transport.js';

test('parseFrame parses a valid JSON object with a type', () => {
  const m = parseFrame(JSON.stringify({ type: 'room-expired' }));
  assert.ok(m);
  assert.equal((m as { type: string }).type, 'room-expired');
});

test('parseFrame returns null for non-string frames', () => {
  assert.equal(parseFrame(Buffer.from('x')), null);
  assert.equal(parseFrame(42), null);
});

test('parseFrame returns null for invalid JSON', () => {
  assert.equal(parseFrame('{not json'), null);
});

test('parseFrame returns null for non-object JSON', () => {
  assert.equal(parseFrame('42'), null);
  assert.equal(parseFrame('"hi"'), null);
  assert.equal(parseFrame('[1,2]'), null);
});

test('parseFrame returns null when type is missing or wrong type', () => {
  assert.equal(parseFrame(JSON.stringify({ foo: 1 })), null);
  assert.equal(parseFrame(JSON.stringify({ type: 123 })), null);
});
