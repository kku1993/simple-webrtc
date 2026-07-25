import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  RoomSessionStore,
  MemorySessionStore,
  type RoomSession,
} from '../src/storage.js';

function sample(over: Partial<RoomSession> = {}): RoomSession {
  return {
    roomId: 'room-abc',
    role: 'host',
    rejoinToken: 'v1.payload.sig',
    hostEpoch: 'host-epoch-1',
    guestEpoch: null,
    ...over,
  };
}

test('MemorySessionStore round-trips a value', () => {
  const s = new MemorySessionStore();
  s.set('k', 'v');
  assert.equal(s.get('k'), 'v');
  s.delete('k');
  assert.equal(s.get('k'), null);
});

test('RoomSessionStore.save then load returns the session', () => {
  const store = new RoomSessionStore(new MemorySessionStore());
  const sess = sample();
  store.save(sess);
  assert.deepEqual(store.load('room-abc'), sess);
});

test('RoomSessionStore.load returns null for unknown room', () => {
  const store = new RoomSessionStore();
  assert.equal(store.load('nope'), null);
});

test('RoomSessionStore.load returns null for corrupt JSON', () => {
  const mem = new MemorySessionStore();
  mem.set('peer-client.room.x', '{not json');
  const store = new RoomSessionStore(mem);
  assert.equal(store.load('x'), null);
});

test('RoomSessionStore.load rejects malformed payloads', () => {
  const mem = new MemorySessionStore();
  mem.set('peer-client.room.bad', JSON.stringify({ roomId: 'bad', role: 'host' }));
  const store = new RoomSessionStore(mem);
  assert.equal(store.load('bad'), null);
});

test('RoomSessionStore.delete removes the record', () => {
  const store = new RoomSessionStore(new MemorySessionStore());
  store.save(sample());
  store.delete('room-abc');
  assert.equal(store.load('room-abc'), null);
});

test('RoomSessionStore supports multiple concurrent rooms', () => {
  const store = new RoomSessionStore(new MemorySessionStore());
  store.save(sample({ roomId: 'r1', role: 'host' }));
  store.save(sample({ roomId: 'r2', role: 'guest', guestEpoch: 'g1' }));
  assert.equal(store.load('r1')?.role, 'host');
  assert.equal(store.load('r2')?.role, 'guest');
  assert.equal(store.load('r2')?.guestEpoch, 'g1');
});
