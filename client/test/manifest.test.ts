// Manifest parsing, weighted shard selection, room-id → shard lookup, the
// caching loader, and the PeerConnection wiring that ties them together.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  ManifestError,
  ManifestProvider,
  applyManifestRtcConfig,
  parseManifest,
  selectShard,
  shardForRoomId,
  singleShardManifest,
  type ManifestFetch,
  type ManifestResponseLike,
  type SignalManifest,
} from '../src/manifest.js';
import { PeerConnection } from '../src/peer-connection.js';
import { createFakeHarness, type FakeHarness } from './fakes.js';

const SHARDS = {
  version: 1,
  shards: [
    { name: 't', url: 'wss://t.example/v1/signal', weight: 1 },
    { name: 'k', url: 'wss://k.example/v1/signal', weight: 3 },
  ],
};

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

test('parseManifest normalizes shard names and defaults the version', () => {
  const m = parseManifest({ shards: [{ name: ' T ', url: ' wss://t.example/v1/signal ' }] });
  assert.equal(m.version, 1);
  assert.deepEqual(m.shards, [{ name: 't', url: 'wss://t.example/v1/signal' }]);
});

test('parseManifest carries ICE settings and opaque app settings', () => {
  const m = parseManifest({
    ...SHARDS,
    iceServers: [
      { urls: 'stun:stun.example:3478' },
      { urls: ['turn:turn.example:3478'], username: 'u', credential: 'p' },
    ],
    iceTransportPolicy: 'relay',
    settings: { maxRoomSize: 2 },
  });
  assert.equal(m.iceServers?.length, 2);
  assert.equal(m.iceServers?.[1]?.username, 'u');
  assert.equal(m.iceTransportPolicy, 'relay');
  assert.deepEqual(m.settings, { maxRoomSize: 2 });
});

test('parseManifest rejects malformed documents with operator-readable errors', () => {
  const cases: [unknown, RegExp][] = [
    ['nope', /must be a JSON object/],
    [{ shards: [] }, /non-empty array/],
    [{ shards: [{ url: 'wss://a/' }] }, /shards\[0\]\.name/],
    [{ shards: [{ name: 't' }] }, /shards\[0\]\.url/],
    [{ shards: [{ name: 't', url: 'https://a/' }] }, /ws:\/\/ or wss:\/\//],
    [{ shards: [{ name: 't', url: 'wss://a/', weight: -1 }] }, /weight/],
    [
      { shards: [{ name: 't', url: 'wss://a/' }, { name: 'T', url: 'wss://b/' }] },
      /duplicate shard name/,
    ],
    [{ ...SHARDS, iceTransportPolicy: 'sometimes' }, /iceTransportPolicy/],
    [{ ...SHARDS, iceServers: [{}] }, /iceServers\[0\]\.urls/],
    [{ ...SHARDS, version: 99 }, /newer than this client supports/],
  ];
  for (const [input, pattern] of cases) {
    assert.throws(() => parseManifest(input), (e: unknown) => {
      assert.ok(e instanceof ManifestError, `expected ManifestError for ${JSON.stringify(input)}`);
      assert.match(e.message, pattern);
      return true;
    });
  }
});

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

test('selectShard divides the range in proportion to weight', () => {
  const m = parseManifest(SHARDS);
  // Weights 1 and 3 over a total of 4: [0, 0.25) → t, [0.25, 1) → k.
  assert.equal(selectShard(m, () => 0).name, 't');
  assert.equal(selectShard(m, () => 0.2499).name, 't');
  assert.equal(selectShard(m, () => 0.25).name, 'k');
  assert.equal(selectShard(m, () => 0.999).name, 'k');
});

test('selectShard skips drained shards but they still serve their room ids', () => {
  const m = parseManifest({
    shards: [
      { name: 't', url: 'wss://t.example/v1/signal', weight: 0 },
      { name: 'k', url: 'wss://k.example/v1/signal', weight: 1 },
    ],
  });
  for (const r of [0, 0.5, 0.9999]) assert.equal(selectShard(m, () => r).name, 'k');
  // A guest holding a room on the drained shard is still routed to it.
  assert.equal(shardForRoomId(m, 't1a230').name, 't');
});

test('selectShard throws when every shard is drained', () => {
  const m = parseManifest({ shards: [{ name: 't', url: 'wss://t.example/', weight: 0 }] });
  assert.throws(() => selectShard(m, () => 0), ManifestError);
});

test('shardForRoomId normalizes the room id before matching', () => {
  const m = parseManifest(SHARDS);
  // Crockford fuzzy decoding is applied first: uppercase and O/I/L folding.
  assert.equal(shardForRoomId(m, 'T1A230').url, 'wss://t.example/v1/signal');
  assert.equal(shardForRoomId(m, 'k9z810').url, 'wss://k.example/v1/signal');
});

test('shardForRoomId prefers the longest matching prefix over the wildcard', () => {
  const m = parseManifest({
    shards: [
      { name: '*', url: 'wss://any.example/v1/signal' },
      { name: 't', url: 'wss://t.example/v1/signal' },
      { name: 't1', url: 'wss://t1.example/v1/signal' },
    ],
  });
  assert.equal(shardForRoomId(m, 't1a230').name, 't1');
  assert.equal(shardForRoomId(m, 't9a230').name, 't');
  assert.equal(shardForRoomId(m, 'z9a230').name, '*');
});

test('shardForRoomId throws when no shard owns the room id', () => {
  const m = parseManifest(SHARDS);
  assert.throws(() => shardForRoomId(m, 'z9a230'), (e: unknown) => {
    assert.ok(e instanceof ManifestError);
    assert.match(e.message, /known shards: t, k/);
    return true;
  });
});

test('singleShardManifest builds a wildcard manifest for one endpoint', () => {
  const m = singleShardManifest('ws://localhost:8080/v1/signal');
  assert.equal(shardForRoomId(m, 'anything').url, 'ws://localhost:8080/v1/signal');
  assert.equal(selectShard(m, () => 0.5).url, 'ws://localhost:8080/v1/signal');
});

test('applyManifestRtcConfig lets locally supplied config win', () => {
  const m = parseManifest({
    ...SHARDS,
    iceServers: [{ urls: 'stun:manifest.example:3478' }],
    iceTransportPolicy: 'relay',
  });
  assert.deepEqual(applyManifestRtcConfig(m, undefined), {
    iceServers: [{ urls: 'stun:manifest.example:3478' }],
    iceTransportPolicy: 'relay',
  });
  const local = { iceServers: [{ urls: 'stun:local.example:3478' }] };
  assert.deepEqual(applyManifestRtcConfig(m, local), {
    iceServers: [{ urls: 'stun:local.example:3478' }],
    iceTransportPolicy: 'relay',
  });
  assert.equal(applyManifestRtcConfig(null, local), local);
});

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

function jsonResponse(body: unknown, status = 200): ManifestResponseLike {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) };
}

interface FakeFetch {
  fetch: ManifestFetch;
  calls: string[];
  reply: (r: ManifestResponseLike | Error) => void;
}

function fakeFetch(initial: ManifestResponseLike | Error): FakeFetch {
  let next = initial;
  const calls: string[] = [];
  return {
    calls,
    reply: (r) => {
      next = r;
    },
    fetch: (url) => {
      calls.push(url);
      return next instanceof Error ? Promise.reject(next) : Promise.resolve(next);
    },
  };
}

test('ManifestProvider parses a static manifest eagerly', () => {
  const p = new ManifestProvider({ static: SHARDS });
  assert.equal(p.manifestUrl, null);
  assert.equal(p.cached?.shards.length, 2);
  assert.throws(() => new ManifestProvider({ static: { shards: [] } }), ManifestError);
});

test('ManifestProvider caches for the TTL and refetches after it', async () => {
  const f = fakeFetch(jsonResponse(SHARDS));
  const p = new ManifestProvider({ url: 'https://cfg.example/m.json', fetch: f.fetch, ttlMs: 50 });
  await p.get();
  await p.get();
  assert.equal(f.calls.length, 1);
  await p.get({ forceRefresh: true });
  assert.equal(f.calls.length, 2);
  assert.deepEqual(f.calls, ['https://cfg.example/m.json', 'https://cfg.example/m.json']);
});

test('ManifestProvider shares one request between concurrent callers', async () => {
  const f = fakeFetch(jsonResponse(SHARDS));
  const p = new ManifestProvider({ url: 'https://cfg.example/m.json', fetch: f.fetch });
  const [a, b] = await Promise.all([p.get(), p.get()]);
  assert.equal(f.calls.length, 1);
  assert.equal(a, b);
});

test('a failed refresh keeps serving the last good manifest', async () => {
  const f = fakeFetch(jsonResponse(SHARDS));
  const p = new ManifestProvider({ url: 'https://cfg.example/m.json', fetch: f.fetch, ttlMs: 0 });
  const first = await p.get();
  f.reply(new Error('network down'));
  const second = await p.get();
  assert.equal(second, first, 'stale manifest is better than no manifest');
});

test('a failed first load uses the fallback manifest when one is configured', async () => {
  const f = fakeFetch(new Error('network down'));
  const p = new ManifestProvider({
    url: 'https://cfg.example/m.json',
    fetch: f.fetch,
    fallback: { shards: [{ name: 't', url: 'ws://localhost:8080/v1/signal' }] },
  });
  const m = await p.get();
  assert.equal(m.shards[0]?.url, 'ws://localhost:8080/v1/signal');
});

test('a failed first load with no fallback rejects', async () => {
  const f = fakeFetch(jsonResponse(null, 503));
  const p = new ManifestProvider({ url: 'https://cfg.example/m.json', fetch: f.fetch });
  await assert.rejects(p.get(), (e: unknown) => {
    assert.ok(e instanceof ManifestError);
    assert.match(e.message, /HTTP 503/);
    return true;
  });
  // The failure is not cached: a later call tries again.
  f.reply(jsonResponse(SHARDS));
  assert.equal((await p.get()).shards.length, 2);
});

// ---------------------------------------------------------------------------
// PeerConnection wiring
// ---------------------------------------------------------------------------

const ISO_FUTURE = new Date(Date.now() + 12 * 3600 * 1000).toISOString();
const ISO_SOON = new Date(Date.now() + 600 * 1000).toISOString();

function waitForSocket(h: FakeHarness, timeoutMs = 1000): Promise<void> {
  const start = Date.now();
  return new Promise((resolve, reject) => {
    const check = (): void => {
      try {
        void h.ws;
        resolve();
      } catch {
        if (Date.now() - start > timeoutMs) return reject(new Error('timeout waiting for socket'));
        setImmediate(check);
      }
    };
    check();
  });
}

function lastSent(h: FakeHarness, type: string): Record<string, unknown> | undefined {
  return [...h.ws.sent]
    .map((s) => JSON.parse(s) as Record<string, unknown>)
    .reverse()
    .find((m) => m['type'] === type);
}

async function waitForSent(h: FakeHarness, type: string, timeoutMs = 1000): Promise<Record<string, unknown>> {
  await waitForSocket(h, timeoutMs);
  const start = Date.now();
  return new Promise((resolve, reject) => {
    const check = (): void => {
      const m = lastSent(h, type);
      if (m) return resolve(m);
      if (Date.now() - start > timeoutMs) return reject(new Error(`timeout waiting for ${type}`));
      setImmediate(check);
    };
    check();
  });
}

test('the host picks a shard by weight and connects to it', async () => {
  const h = createFakeHarness();
  const conn = new PeerConnection({
    manifest: { static: SHARDS },
    shardRandom: () => 0.5, // lands in the weight-3 'k' bucket
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  const created = conn.createRoom();
  const msg = await waitForSent(h, 'create-room');
  assert.equal(h.ws.url, 'wss://k.example/v1/signal');
  h.ws.receiveMessage({
    type: 'create-room-response',
    roomId: 'k1a230',
    role: 'host',
    rejoinToken: 'tok',
    hostEpoch: msg['hostEpoch'],
    peerDeadlineAt: ISO_SOON,
    peerDeadlineInSeconds: 600,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
  });
  const res = await created;
  assert.equal(res.roomId, 'k1a230');
  assert.equal(conn.currentShard?.name, 'k');
  assert.equal(conn.signalUrl, 'wss://k.example/v1/signal');
  conn.destroy();
});

test('the guest connects to the shard that owns the room id', async () => {
  const h = createFakeHarness();
  const conn = new PeerConnection({
    manifest: { static: SHARDS },
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  const joined = conn.joinRoom({ roomId: 'T1A230' }); // user-typed, unnormalized
  const msg = await waitForSent(h, 'join-room');
  assert.equal(h.ws.url, 'wss://t.example/v1/signal');
  assert.equal(msg['roomId'], 't1a230');
  h.ws.receiveMessage({
    type: 'join-room-response',
    roomId: 't1a230',
    role: 'guest',
    rejoinToken: 'tok',
    hostEpoch: 'he',
    guestEpoch: msg['guestEpoch'],
    hostConnected: true,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
  });
  await joined;
  assert.equal(conn.currentShard?.name, 't');
  conn.destroy();
});

test('joining a room whose shard is unknown fails before any socket is opened', async () => {
  const h = createFakeHarness();
  const conn = new PeerConnection({
    manifest: { static: SHARDS },
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  await assert.rejects(conn.joinRoom({ roomId: 'z9a230' }), ManifestError);
  assert.throws(() => h.ws, /transport not opened yet/);
  conn.destroy();
});

test('a remote manifest is fetched before the first handshake', async () => {
  const h = createFakeHarness();
  const f = fakeFetch(jsonResponse(SHARDS));
  const conn = new PeerConnection({
    manifest: { url: 'https://cfg.example/m.json', fetch: f.fetch },
    shardRandom: () => 0,
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  void conn.createRoom().catch(() => undefined);
  await waitForSent(h, 'create-room');
  assert.deepEqual(f.calls, ['https://cfg.example/m.json']);
  assert.equal(h.ws.url, 'wss://t.example/v1/signal');
  conn.destroy();
});

test('manifest ICE servers reach every peer generation', async () => {
  const h = createFakeHarness();
  const conn = new PeerConnection({
    manifest: {
      static: {
        ...SHARDS,
        iceServers: [{ urls: 'turn:turn.example:3478', username: 'u', credential: 'p' }],
        iceTransportPolicy: 'relay',
      },
    },
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  const joined = conn.joinRoom({ roomId: 't1a230' });
  const msg = await waitForSent(h, 'join-room');
  h.ws.receiveMessage({
    type: 'join-room-response',
    roomId: 't1a230',
    role: 'guest',
    rejoinToken: 'tok',
    hostEpoch: 'he',
    guestEpoch: msg['guestEpoch'],
    hostConnected: true,
    roomExpiresAt: ISO_FUTURE,
    roomExpiresInSeconds: 5400,
    rejoinTokenExpiresAt: ISO_FUTURE,
  });
  await joined;
  const config = h.peers[0]?.opts.config;
  assert.deepEqual(config?.iceServers, [
    { urls: 'turn:turn.example:3478', username: 'u', credential: 'p' },
  ]);
  assert.equal(config?.iceTransportPolicy, 'relay');
  conn.destroy();
});

test('url and manifest are mutually exclusive, and one is required', () => {
  const bases = { transportFactory: createFakeHarness().transportFactory };
  assert.throws(
    () => new PeerConnection({ ...bases, url: 'wss://a/', manifest: { static: SHARDS } }),
    TypeError,
  );
  assert.throws(() => new PeerConnection({ ...bases }), TypeError);
});

test('a plain url behaves as a one-wildcard-shard manifest', async () => {
  const h = createFakeHarness();
  const conn = new PeerConnection({
    url: 'ws://localhost:8080/v1/signal',
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  void conn.joinRoom({ roomId: 'zzz999' });
  await waitForSent(h, 'join-room');
  assert.equal(h.ws.url, 'ws://localhost:8080/v1/signal');
  assert.equal(conn.currentShard?.name, '*');
  conn.destroy();
});

// A manifest whose shard set changes between the host's create and a later
// reconnect must not move an existing room: the shard is pinned for the life of
// the connection.
test('the shard is pinned for the life of the connection', async () => {
  const h = createFakeHarness();
  const manifest: SignalManifest = parseManifest(SHARDS);
  const conn = new PeerConnection({
    manifest: { static: manifest },
    shardRandom: () => 0,
    transportFactory: h.transportFactory,
    peerFactory: h.peerFactory,
  });
  void conn.createRoom();
  await waitForSent(h, 'create-room');
  const first = conn.currentShard;
  // Even a fresh selection roll cannot move the pinned shard.
  void conn.createRoom().catch(() => undefined);
  assert.equal(conn.currentShard, first);
  conn.destroy();
});
