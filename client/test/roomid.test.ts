import { test } from 'node:test';
import assert from 'node:assert/strict';
import { normalizeRoomId } from '../src/roomid.js';

test('normalizeRoomId lowercases alphabetic characters', () => {
  assert.equal(normalizeRoomId('TA0000'), 'ta0000');
  assert.equal(normalizeRoomId('AbCdEf'), 'abcdef');
});

test('normalizeRoomId maps O→0 (case-insensitive)', () => {
  assert.equal(normalizeRoomId('tO0000'), 't00000');
  assert.equal(normalizeRoomId('TO0000'), 't00000');
  assert.equal(normalizeRoomId('tooooo'), 't00000');
});

test('normalizeRoomId maps I→1 and L→1 (case-insensitive)', () => {
  assert.equal(normalizeRoomId('tI0000'), 't10000');
  assert.equal(normalizeRoomId('tL0000'), 't10000');
  assert.equal(normalizeRoomId('tiiiii'), 't11111');
  assert.equal(normalizeRoomId('tlllll'), 't11111');
  assert.equal(normalizeRoomId('tILiLl'), 't11111');
});

test('normalizeRoomId leaves digits and base32 chars unchanged (after lowercasing)', () => {
  assert.equal(normalizeRoomId('ta0000'), 'ta0000');
  assert.equal(normalizeRoomId('tz1234'), 'tz1234');
  assert.equal(normalizeRoomId('tjkmnp'), 'tjkmnp');
});

test('normalizeRoomId does NOT reject malformed ids — passes them through', () => {
  // Wrong length, unknown characters, separators — all flow through to the
  // backend, which owns validation per docs/ROOM_ID_SPEC.md §"Frontend handling".
  assert.equal(normalizeRoomId(''), '');
  assert.equal(normalizeRoomId('x'), 'x');
  assert.equal(normalizeRoomId('ta000'), 'ta000');
  assert.equal(normalizeRoomId('ta000000'), 'ta000000');
  assert.equal(normalizeRoomId('t_0000'), 't_0000');
  assert.equal(normalizeRoomId('t-0000'), 't-0000');
  assert.equal(normalizeRoomId('t!@#$%'), 't!@#$%');
  assert.equal(normalizeRoomId('TA-0000'), 'ta-0000');
});

test('normalizeRoomId is a pure per-character transform (no length check)', () => {
  // The transform must be safe to apply broadly without knowing the schema,
  // so the schema can change on the backend without a frontend update.
  assert.equal(normalizeRoomId('O'), '0');
  assert.equal(normalizeRoomId('L'), '1');
  assert.equal(normalizeRoomId('I'), '1');
  assert.equal(normalizeRoomId('A'), 'a');
});

test('normalizeRoomId handles a realistic uppercase user-entered id', () => {
  // User types "TOl234" expecting "ta0000"-style — fuzzy decoding yields the
  // canonical form the backend will recognize.
  assert.equal(normalizeRoomId('TOl234'), 't01234');
  assert.equal(normalizeRoomId('TAOOOO'), 'ta0000');
});
