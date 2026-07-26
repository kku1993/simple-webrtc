// Test helpers: a fake WebSocket and a fake simple-peer that let us drive the
// protocol state machine deterministically without a browser or `wrtc`.

import type { WebSocketLike } from '../src/transport.js';
import type { PeerLike, SimplePeerOptions } from '../src/peer-connection.js';
import type { SimplePeerSignalData } from '../src/types.js';

type Listener = (...args: any[]) => void;

/**
 * A minimal WebSocket double. Tests call `server.receive(jsonString)` to
 * deliver a frame and `server.close(code, reason)` to simulate a server-side
 * close. Frames the client sends are captured in `sent`.
 */
export class FakeWebSocket implements WebSocketLike {
  readonly sent: string[] = [];
  readyState = 0; // CONNECTING
  private listeners = new Map<string, Set<Listener>>();

  constructor(readonly url: string) {
    // Auto-open on next tick to mimic a real upgrade.
    queueMicrotask(() => {
      this.readyState = 1; // OPEN
      this.emit('open');
    });
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(code?: number, reason?: string): void {
    if (this.readyState === 3) return;
    this.readyState = 3; // CLOSED
    this.emit('close', { code: code ?? 1000, reason: reason ?? '' });
  }

  addEventListener(event: string, handler: Listener): void {
    let set = this.listeners.get(event);
    if (!set) {
      set = new Set<Listener>();
      this.listeners.set(event, set);
    }
    set.add(handler);
  }

  removeEventListener(event: string, handler: Listener): void {
    this.listeners.get(event)?.delete(handler);
  }

  // --- test driver API -----------------------------------------------------
  receive(json: string): void {
    this.emit('message', { data: json });
  }

  receiveMessage(obj: unknown): void {
    this.receive(JSON.stringify(obj));
  }

  closeFromServer(code: number, reason = ''): void {
    this.readyState = 3;
    this.emit('close', { code, reason });
  }

  emitError(err: unknown): void {
    this.emit('error', err);
  }

  private emit(event: string, ...args: unknown[]): void {
    for (const h of [...(this.listeners.get(event) ?? [])]) h(...args);
  }
}

/**
 * A fake `simple-peer` instance. It records `signal`/`send` calls and lets
 * tests fire `signal`/`connect`/`data`/`close`/`error` events via the
 * `emit*` methods. Media methods (`addTrack`/`removeTrack`/`replaceTrack`/
 * `addStream`/`removeStream`) record their arguments so tests can assert
 * attachment behavior across peer generations.
 */
export class FakePeer implements PeerLike {
  readonly signals: (string | SimplePeerSignalData)[] = [];
  readonly sent: unknown[] = [];
  /** Recorded `addTrack(track, stream)` calls in order. */
  readonly addTrackCalls: { track: MediaStreamTrack; stream: MediaStream }[] = [];
  /** Recorded `removeTrack(track, stream)` calls in order. */
  readonly removeTrackCalls: { track: MediaStreamTrack; stream: MediaStream }[] = [];
  /** Recorded `replaceTrack(old, new, stream)` calls in order. */
  readonly replaceTrackCalls: {
    oldTrack: MediaStreamTrack;
    newTrack: MediaStreamTrack;
    stream: MediaStream;
  }[] = [];
  /** Recorded `addStream(stream)` calls in order. */
  readonly addStreamCalls: { stream: MediaStream }[] = [];
  /** Recorded `removeStream(stream)` calls in order. */
  readonly removeStreamCalls: { stream: MediaStream }[] = [];
  /** Optional override: when set, `addTrack` throws this error. */
  addTrackThrows: Error | null = null;
  /** Optional override: when set, `removeTrack` throws this error. */
  removeTrackThrows: Error | null = null;
  /** Optional override: when set, `replaceTrack` rejects/throws this error. */
  replaceTrackThrows: Error | null = null;
  /** Optional override: when set, `replaceTrack` returns this Promise. */
  replaceTrackReturns: Promise<void> | null = null;
  connected = false;
  destroyed = false;
  readonly initiator: boolean;
  private listeners = new Map<string, Set<Listener>>();

  constructor(opts: SimplePeerOptions & { initiator: boolean }) {
    this.initiator = opts.initiator;
  }

  signal(data: string | SimplePeerSignalData): void {
    this.signals.push(data);
  }

  send(data: unknown): void {
    this.sent.push(data);
  }

  destroy(_error?: Error): unknown {
    this.destroyed = true;
    return undefined;
  }

  addTrack(track: MediaStreamTrack, stream: MediaStream): void {
    if (this.addTrackThrows) throw this.addTrackThrows;
    this.addTrackCalls.push({ track, stream });
  }

  removeTrack(track: MediaStreamTrack, stream: MediaStream): void {
    if (this.removeTrackThrows) throw this.removeTrackThrows;
    this.removeTrackCalls.push({ track, stream });
  }

  replaceTrack(
    oldTrack: MediaStreamTrack,
    newTrack: MediaStreamTrack,
    stream: MediaStream,
  ): Promise<void> | void {
    if (this.replaceTrackThrows) throw this.replaceTrackThrows;
    this.replaceTrackCalls.push({ oldTrack, newTrack, stream });
    return this.replaceTrackReturns ?? undefined;
  }

  addStream(stream: MediaStream): void {
    this.addStreamCalls.push({ stream });
  }

  removeStream(stream: MediaStream): void {
    this.removeStreamCalls.push({ stream });
  }

  on(event: string, fn: Listener): this {
    let set = this.listeners.get(event);
    if (!set) {
      set = new Set<Listener>();
      this.listeners.set(event, set);
    }
    set.add(fn);
    return this;
  }

  // --- test driver API -----------------------------------------------------
  emitSignal(data: SimplePeerSignalData): void {
    this.fire('signal', data);
  }

  emitConnect(): void {
    this.connected = true;
    this.fire('connect');
  }

  emitData(chunk: unknown): void {
    this.fire('data', chunk);
  }

  emitClose(): void {
    this.connected = false;
    this.fire('close');
  }

  emitError(err: Error): void {
    this.fire('error', err);
  }

  /** Fire a `stream` event to listeners. */
  emitStream(stream: MediaStream): void {
    this.fire('stream', stream);
  }

  /** Fire a `track` event to listeners. */
  emitTrack(track: MediaStreamTrack, stream: MediaStream): void {
    this.fire('track', track, stream);
  }

  private fire(event: string, ...args: unknown[]): void {
    for (const h of [...(this.listeners.get(event) ?? [])]) h(...args);
  }
}

// ---------------------------------------------------------------------------
// Fake MediaStreamTrack / MediaStream
//
// Node does not expose browser media globals, so tests use minimal stubs that
// implement just the surface the wrapper touches (`id`, `kind`, `readyState`,
// `getTracks()`). They are typed as the real DOM types via `as unknown as`
// casts at call sites.
// ---------------------------------------------------------------------------

export class FakeTrack {
  readonly id: string;
  readonly kind: string;
  readyState: MediaStreamTrackState;
  /** Set to true to simulate `track.enabled`. */
  enabled = true;

  constructor(opts: { id?: string; kind?: string; readyState?: MediaStreamTrackState } = {}) {
    this.id = opts.id ?? `track-${Math.random().toString(36).slice(2, 10)}`;
    this.kind = opts.kind ?? 'audio';
    this.readyState = opts.readyState ?? 'live';
  }

  /** Mimic `MediaStreamTrack.stop()`. */
  stop(): void {
    this.readyState = 'ended';
  }
}

export class FakeStream {
  readonly id: string;
  private readonly tracks: FakeTrack[] = [];

  constructor(tracks: FakeTrack[] = []) {
    this.id = `stream-${Math.random().toString(36).slice(2, 10)}`;
    for (const t of tracks) this.tracks.push(t);
  }

  getTracks(): FakeTrack[] {
    return [...this.tracks];
  }

  addTrack(t: FakeTrack): void {
    this.tracks.push(t);
  }
}

/** Cast a {@link FakeTrack} to the DOM `MediaStreamTrack` type. */
export function asTrack(t: FakeTrack): MediaStreamTrack {
  return t as unknown as MediaStreamTrack;
}

/** Cast a {@link FakeStream} to the DOM `MediaStream` type. */
export function asStream(s: FakeStream): MediaStream {
  return s as unknown as MediaStream;
}

/** Build a transportFactory and simplePeerFactory that use the fakes above. */
export interface FakeHarness {
  ws: FakeWebSocket;
  peers: FakePeer[];
  transportFactory: (url: string) => WebSocketLike;
  simplePeerFactory: (opts: SimplePeerOptions & { initiator: boolean }) => PeerLike;
}

export function createFakeHarness(): FakeHarness {
  const peers: FakePeer[] = [];
  let ws: FakeWebSocket | null = null;
  return {
    get ws(): FakeWebSocket {
      if (!ws) throw new Error('transport not opened yet');
      return ws;
    },
    peers,
    transportFactory: (url) => {
      ws = new FakeWebSocket(url);
      return ws;
    },
    simplePeerFactory: (opts) => {
      const p = new FakePeer(opts);
      peers.push(p);
      return p;
    },
  };
}
