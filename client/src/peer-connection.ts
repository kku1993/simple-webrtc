import {
  type ClientMessage,
  type CreateRoomResponse,
  type ErrorResponseMessage,
  type JoinRoomResponse,
  type RejoinRoomResponse,
  type Role,
  type ServerMessage,
  type SignalResponseMessage,
  CloseCode as CloseCodeEnum,
} from './types.js';
import { Emitter } from './emitter.js';
import { SignalingError } from './errors.js';
import type { Logger } from './logger.js';
import { type RoomSession, type SessionStore, RoomSessionStore, MemorySessionStore } from './storage.js';
import { Transport, type WebSocketFactory, type WebSocketLike } from './transport.js';
import { SequenceCounter, generateEpoch, generateRequestId, fullJitterBackoff } from './util.js';
import { normalizeRoomId } from './roomid.js';
import { RtcPeer } from './rtc/peer.js';
import { DataChannelHandle } from './rtc/channel-handle.js';
import { assertUsableLabel, resolveSpec } from './rtc/channels.js';
import type { SignalData } from './rtc/signal.js';
import type {
  ChannelDiagnostics,
  DataChannelSpec,
  RtcPeerEvents,
  RtcPeerOptions,
} from './rtc/types.js';

export { SignalingError, DataChannelNotOpenError } from './errors.js';
export type { Logger } from './logger.js';
export * from './types.js';
export * from './storage.js';
export type { WebSocketFactory, WebSocketLike } from './transport.js';

// ---------------------------------------------------------------------------
// Public option / result types
// ---------------------------------------------------------------------------

/**
 * Options forwarded to every internal {@link RtcPeer}. `initiator` is derived
 * from the protocol role, and the channel registry is owned by this class, so
 * neither is settable here.
 */
export type RtcOptions = Omit<
  RtcPeerOptions,
  'initiator' | 'channelHandles' | 'dataChannels' | 'streams'
>;

/**
 * The slice of {@link RtcPeer}'s API that {@link PeerConnection} relies on.
 *
 * Declared structurally so tests can inject a stub without a browser or any
 * WebRTC implementation. Note that `RtcPeer` also accepts an `rtcImpl` override,
 * which is the better seam when you want to exercise the real engine against a
 * fake `RTCPeerConnection`; this interface exists for tests that want to drive
 * the protocol state machine without an engine at all.
 */
export interface RtcPeerLike {
  readonly connected: boolean;
  on<K extends keyof RtcPeerEvents>(event: K, fn: (arg: RtcPeerEvents[K]) => void): unknown;
  /** Apply an inbound signal frame. Unrecognized frames are ignored, not fatal. */
  signal(data: unknown): void;
  /** Send on the default application channel. */
  send(data: string | Blob | ArrayBuffer | ArrayBufferView): void;
  /** Send a signal frame over the control channel. Returns false if it is not open. */
  sendControlSignal(data: SignalData): boolean;
  destroy(error?: Error): void;
  /** Handle for a declared or dynamic channel. */
  channel(label: string): DataChannelHandle;
  /** Open a channel not present in the declared set. */
  openChannel(label: string, spec?: DataChannelSpec): DataChannelHandle;
  addTrack(track: MediaStreamTrack, stream: MediaStream): void;
  removeTrack(track: MediaStreamTrack, stream: MediaStream): void;
  replaceTrack(
    oldTrack: MediaStreamTrack,
    newTrack: MediaStreamTrack,
    stream: MediaStream,
  ): Promise<void>;
  addStream(stream: MediaStream): void;
  removeStream(stream: MediaStream): void;
  getStats(): Promise<RTCStatsReport>;
  /** Per-channel diagnostics. */
  readonly channelDiagnostics?: ChannelDiagnostics[];
}

/** Factory used to construct each internal peer. Overridable for tests. */
export type RtcPeerFactory = (opts: RtcPeerOptions) => RtcPeerLike;

export interface PeerConnectionOptions {
  /** WebSocket URL of the signaling endpoint, e.g. `wss://host/v1/signal`. */
  url: string;
  /** Options forwarded to every internal {@link RtcPeer} (ICE config, SDP hooks). */
  rtc?: RtcOptions;
  /**
   * Application data channels to open, keyed by label.
   *
   * Both peers should declare the same set. The host creates them and the guest
   * binds by label, so a channel's ordering and reliability always come from the
   * host and the two sides cannot disagree about them.
   *
   * ```ts
   * dataChannels: {
   *   chat: {},                                    // ordered, reliable
   *   cursor: { ordered: false, maxRetransmits: 0 } // unordered, fire-and-forget
   * }
   * ```
   *
   * Handles are available from {@link PeerConnection.channel} immediately, before
   * any room exists, and keep their identity across peer rebuilds.
   */
  dataChannels?: Record<string, DataChannelSpec>;
  /** Persistence for `{ roomId, role, rejoinToken, epochs }`. Defaults to in-memory. */
  store?: SessionStore;
  /** Override the WebSocket constructor (mainly for tests). */
  transportFactory?: WebSocketFactory;
  /** Override the internal peer constructor (mainly for tests). */
  peerFactory?: RtcPeerFactory;
  /** Structured logger; defaults to no-op. */
  logger?: Logger;
  /**
   * Maximum reconnect attempts before surfacing a terminal error. Defaults to
   * `Infinity` — the client keeps trying until the rejoin token expires or the
   * user closes it.
   */
  maxReconnectAttempts?: number;
  /**
   * Optional local `MediaStream` to attach to every internal peer generation.
   * Equivalent to calling {@link PeerConnection.setLocalStream} before
   * `createRoom`/`joinRoom`. Useful when media is acquired before the
   * connection is constructed (e.g. from a permissions prompt triggered by a
   * user gesture in the lobby).
   */
  localStream?: MediaStream | null;
}

export interface RoomState {
  roomId: string;
  role: Role;
  rejoinToken: string;
  hostEpoch: string | null;
  guestEpoch: string | null;
  /** ISO deadline by which the peer must join, or null when both slots are filled. */
  peerDeadlineAt: string | null;
  /** ISO deadline at which the in-memory room instance expires. */
  roomExpiresAt: string;
  /** ISO deadline at which the rejoin token expires (pairing cannot be rebuilt after). */
  rejoinTokenExpiresAt: string;
}

export interface CreateRoomResult {
  roomId: string;
  rejoinToken: string;
  peerDeadlineInSeconds: number;
  roomExpiresInSeconds: number;
  rejoinTokenExpiresAt: string;
  state: RoomState;
}

export interface JoinRoomResult {
  rejoinToken: string;
  hostConnected: boolean;
  roomExpiresInSeconds: number;
  rejoinTokenExpiresAt: string;
  state: RoomState;
}

export interface RejoinResult {
  recreated: boolean;
  peerConnected: boolean;
  state: RoomState;
}

/** High-level status the UI can render. */
export type PeerStatus =
  | 'idle'
  | 'connecting'
  | 'waiting-for-peer'
  | 'signaling'
  | 'connected'
  | 'reconnecting'
  | 'closed'
  | 'error';

/**
 * Snapshot of media-related state for diagnostics. Intentionally avoids
 * exposing SDP, device labels, IP addresses, or other sensitive details.
 * Returned by {@link PeerConnection.mediaDiagnostics}.
 */
export interface MediaDiagnostics {
  /** Current internal peer generation (0 before the first peer is built). */
  peerGeneration: number;
  /** Whether an internal peer currently exists. */
  hasPeer: boolean;
  /** Whether the internal peer reports `connected`. */
  peerConnected: boolean;
  /** Number of desired tracks currently retained by the wrapper. */
  desiredTrackCount: number;
  /** Kinds and ready states of desired tracks (no labels). */
  desiredTracks: { kind: string; readyState: MediaStreamTrackState; id: string }[];
  /** Whether a local stream is currently selected via `setLocalStream`. */
  hasLocalStream: boolean;
}

export interface PeerConnectionEvents {
  /** Room state changed (epochs, deadlines, role). */
  state: RoomState;
  /** High-level status transition. */
  status: PeerStatus;
  /** Host only: the guest joined. The SimplePeer is being constructed now. */
  'guest-joined': { guestEpoch: string };
  /** The underlying peer connected (P2P established). */
  connect: void;
  /** Data received on the default data channel. */
  data: unknown;
  /** A remote media stream was added. */
  stream: MediaStream;
  /** A remote track was added. */
  track: { track: MediaStreamTrack; stream: MediaStream };
  /** A declared or dynamic data channel opened. */
  'channel-open': { label: string; channel: DataChannelHandle };
  /** A data channel closed, including via a peer rebuild. */
  'channel-close': { label: string };
  /** A message arrived on a named channel other than the default one. */
  'channel-message': { label: string; data: unknown };
  /** The remote peer opened a channel this side had not declared. */
  channel: { label: string; channel: DataChannelHandle };
  /**
   * A data channel failure. Non-fatal and scoped to one channel — the
   * connection, media, and other channels are unaffected. Distinct from
   * `error` (room/signaling) and `media-error` (tracks).
   */
  'channel-error': { label: string; message: string; cause?: unknown };
  /**
   * A new internal peer generation was constructed. Tracks registered through
   * {@link PeerConnection.addTrack} / {@link PeerConnection.setLocalStream} have
   * already been attached when this fires. Use this for advanced cases that need
   * the raw peer (transceivers, sender parameters, `getStats`).
   */
  'peer-created': { peer: RtcPeerLike; generation: number };
  /**
   * An internal peer generation was destroyed (peer rebuild, close, or terminal
   * error). `reason` is one of `'rebuild'`, `'close'`, `'user-close'`,
   * `'terminal'`.
   */
  'peer-destroyed': { generation: number; reason: PeerDestroyedReason };
  /** The other side's socket dropped (P2P may survive). */
  'peer-disconnected': { role: Role };
  /** The other side reattached with the same epoch (WebRTC session continued). */
  'peer-rejoined': { role: Role; epoch: string };
  /** The other side reattached with a new epoch; the local SimplePeer was rebuilt. */
  'peer-reset': { role: Role; epoch: string };
  /** The server released the sockets after both peers connected (close 4200). Not an error. */
  'socket-released': void;
  /** The room was permanently closed (`close-room` or `room-closed`). */
  'room-closed': { reason: string };
  /** The room expired or hit the peer deadline. Rejoin may still be possible while the token is valid. */
  'room-expired': void;
  /** The server is draining; reconnect will be attempted after the indicated delay. */
  'server-shutdown': { reconnectAfterMs: number };
  /** An error that is not terminal enough to close the connection (e.g. a non-fatal error-response). */
  error: SignalingError;
  /**
   * A media operation failed (e.g. `addTrack` threw on the underlying peer, or
   * `replaceTrack` rejected). Distinct from `error` so applications can
   * surface media negotiation failures without confusing them with
   * room/signaling failures. The connection itself is unaffected.
   */
  'media-error': { message: string; cause?: unknown };
  /** The peer connection has fully closed. */
  close: { reason: string };
}

/** Why an internal `SimplePeer` generation was destroyed. */
export type PeerDestroyedReason = 'rebuild' | 'close' | 'user-close' | 'terminal';

// ---------------------------------------------------------------------------
// Internal handshake descriptor
// ---------------------------------------------------------------------------

type HandshakeKind = 'create' | 'join' | 'rejoin';

interface HandshakeRequest {
  kind: HandshakeKind;
  requestId: string;
  message: ClientMessage;
  /** Resolves with the matching response message. */
  resolve: (msg: ServerMessage) => void;
  reject: (err: SignalingError) => void;
}

/**
 * One entry in the desired-media registry. `attachedGenerations` records which
 * internal `SimplePeer` generations have already received this track, so a
 * single peer never gets the same track twice but a rebuilt peer is re-attached.
 */
interface DesiredTrackEntry {
  track: MediaStreamTrack;
  stream: MediaStream;
  attachedGenerations: Set<number>;
}

// ---------------------------------------------------------------------------
// PeerConnection
// ---------------------------------------------------------------------------

/**
 * Wraps `simple-peer` and speaks the signaling protocol from `docs/DESIGN.md`.
 *
 * One instance represents one room pairing. The host creates a room with
 * {@link PeerConnection.createRoom}; the guest joins with
 * {@link PeerConnection.joinRoom}. After `connect` fires, the server releases
 * its sockets and further renegotiation flows over the data channel.
 *
 * Recovery from a dropped WebSocket, a server restart, or a page reload is
 * automatic and token-based: on reconnect the client sends `rejoin-room` with
 * a fresh epoch, and rebuilds its `SimplePeer` whenever the peer's epoch
 * changed.
 */
export class PeerConnection extends Emitter<PeerConnectionEvents> {
  private readonly opts: {
    url: string;
    maxReconnectAttempts: number;
  };
  private readonly rtcOpts: RtcOptions;
  private readonly declaredChannels: Record<string, DataChannelSpec>;
  private readonly sessionStore: RoomSessionStore;
  private readonly transport: Transport;
  private readonly log: Logger;
  private readonly createPeer: RtcPeerFactory;

  /** Current room state, or null before a successful handshake. */
  private state: RoomState | null = null;
  /** The underlying peer instance, when one exists. */
  private peer: RtcPeerLike | null = null;
  private seq = new SequenceCounter();
  /** True once the server has released our socket (close 4200). */
  private socketReleased = false;
  /** True while the user has asked to close; suppresses reconnect. */
  private userClosed = false;
  /** True once a terminal event has fired (room-closed / room-expired / replaced). */
  private terminal = false;

  private reconnectAttempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private pendingHandshake: HandshakeRequest | null = null;
  private status: PeerStatus = 'idle';

  /**
   * Monotonically increasing generation counter for internal `SimplePeer`
   * instances. Bumped every time a peer is constructed; never reset. Exposed
   * via {@link PeerConnection.peerGeneration} and the `peer-created` /
   * `peer-destroyed` events so consumers can correlate media state across
   * rebuilds.
   */
  private peerGenerationField = 0;

  /**
   * Desired-media registry. Each entry records a track and the stream it
   * should be attached to. Entries survive peer construction, destruction, and
   * rebuild so that tracks registered before `guest-joined` (host) or before
   * `joinRoom` resolves (guest) are attached to the first `SimplePeer`, and so
   * that live tracks are re-attached to a replacement peer after `peer-reset`.
   *
   * Deduplication is by track identity (`MediaStreamTrack.id`), since
   * `addTrack()` returns `void` and cannot itself indicate whether
   * attachment succeeded. A `Set` of peer generations that have already seen
   * a given track prevents double-attachment to a single peer.
   */
  private readonly desiredTracks = new Map<string, DesiredTrackEntry>();
  /**
   * The currently selected local stream (set via {@link PeerConnection.setLocalStream}
   * or the `localStream` constructor option). Its tracks are also kept in
   * {@link desiredTracks} so they survive rebuilds; the reference is retained
   * so a `null` argument can remove all of them at once.
   */
  private localStream: MediaStream | null = null;

  /**
   * Stable data channel handles, keyed by label.
   *
   * Owned here rather than by the internal peer, because the peer is thrown
   * away and rebuilt on every epoch change while the application's
   * `channel('chat')` reference is not. Each new peer generation binds fresh
   * `RTCDataChannel`s into these same objects.
   */
  private readonly channelHandles = new Map<string, DataChannelHandle>();

  constructor(opts: PeerConnectionOptions) {
    super();
    this.opts = {
      url: opts.url,
      maxReconnectAttempts: opts.maxReconnectAttempts ?? Infinity,
    };
    this.rtcOpts = opts.rtc ?? {};
    this.declaredChannels = opts.dataChannels ?? {};
    this.sessionStore = new RoomSessionStore(opts.store ?? new MemorySessionStore());
    this.log = opts.logger ?? {};
    this.createPeer = opts.peerFactory ?? ((o) => new RtcPeer(o));
    this.transport = new Transport(opts.transportFactory);

    // Materialize handles up front so `channel()` works before any room exists
    // — the same contract the media API offers.
    for (const [label, spec] of Object.entries(this.declaredChannels)) {
      assertUsableLabel(label);
      this.channelHandles.set(label, new DataChannelHandle(label, resolveSpec(spec)));
    }

    if (opts.localStream) this.setLocalStream(opts.localStream);
    this.wireTransport();
  }

  // -------------------------------------------------------------------------
  // Public surface
  // -------------------------------------------------------------------------

  /** The current room id, once a handshake has succeeded. */
  get roomId(): string | undefined {
    return this.state?.roomId;
  }

  /** The current role, once a handshake has succeeded. */
  get role(): Role | undefined {
    return this.state?.role;
  }

  /** The current high-level status. */
  get currentStatus(): PeerStatus {
    return this.status;
  }

  /**
   * The underlying {@link RtcPeer}, when one has been constructed.
   *
   * Most applications should use the wrapper's own media and channel methods,
   * which survive peer rebuilds; this is the escape hatch for advanced cases
   * (transceivers, sender parameters, `peerConnection` access).
   *
   * The instance is replaced on every peer rebuild, so callers must re-fetch it
   * after `peer-created` / `peer-destroyed` rather than caching it.
   */
  get peerInstance(): RtcPeerLike | null {
    return this.peer;
  }

  /** True once the P2P connection is established. */
  get connected(): boolean {
    return this.peer?.connected ?? false;
  }

  /** The current room state snapshot, or null. */
  get roomState(): RoomState | null {
    return this.state ? { ...this.state } : null;
  }

  /**
   * The current internal `SimplePeer` generation. Starts at 0 (no peer built
   * yet) and is bumped every time a peer is constructed. Useful for
   * correlating `peer-created` / `peer-destroyed` events with media state.
   */
  get peerGeneration(): number {
    return this.peerGenerationField;
  }

  /**
   * The local `MediaStream` currently selected via {@link setLocalStream} or
   * the `localStream` constructor option, or `null` if none is set. The
   * wrapper does not own the stream or stop its tracks; the application
   * remains responsible for cleanup.
   */
  get currentLocalStream(): MediaStream | null {
    return this.localStream;
  }

  /**
   * Host entry point. Creates a room and takes the host slot. Resolves once
   * the server returns `create-room-response`; the `SimplePeer` is not built
   * yet — the host must wait for the `guest-joined` event.
   */
  async createRoom(input: {
    guestPassword?: string;
    cloudflareTurnstileToken?: string;
  } = {}): Promise<CreateRoomResult> {
    const hostEpoch = generateEpoch();
    const message: ClientMessage = {
      type: 'create-room',
      hostEpoch,
      ...(input.guestPassword !== undefined ? { guestPassword: input.guestPassword } : {}),
      ...(input.cloudflareTurnstileToken !== undefined
        ? { cloudflareTurnstileToken: input.cloudflareTurnstileToken }
        : {}),
    };
    const res = (await this.handshake('create', message)) as CreateRoomResponse;
    this.applyHandshakeResponse(res, { myEpoch: hostEpoch });
    return {
      roomId: res.roomId,
      rejoinToken: res.rejoinToken,
      peerDeadlineInSeconds: res.peerDeadlineInSeconds,
      roomExpiresInSeconds: res.roomExpiresInSeconds,
      rejoinTokenExpiresAt: res.rejoinTokenExpiresAt,
      state: this.snapshotState(res),
    };
  }

  /**
   * Guest entry point. Joins an existing room and takes the guest slot.
   * Resolves once the server returns `join-room-response`; the `SimplePeer`
   * (as non-initiator) is constructed immediately afterwards.
   */
  async joinRoom(input: {
    roomId: string;
    guestPassword?: string;
  }): Promise<JoinRoomResult> {
    const guestEpoch = generateEpoch();
    // Normalize the user-entered room id via Crockford base32 fuzzy decoding
    // (case-insensitive, O→0, I→1, L→1) without rejecting malformed ids —
    // the backend owns validation. See docs/ROOM_ID_SPEC.md §"Frontend handling".
    const roomId = normalizeRoomId(input.roomId);
    const message: ClientMessage = {
      type: 'join-room',
      roomId,
      guestEpoch,
      ...(input.guestPassword !== undefined ? { guestPassword: input.guestPassword } : {}),
    };
    const res = (await this.handshake('join', message)) as JoinRoomResponse;
    this.applyHandshakeResponse(res, { myEpoch: guestEpoch });
    // Guest builds its SimplePeer immediately on join (it is the non-initiator).
    this.maybeBuildPeer({ initiator: false });
    return {
      rejoinToken: res.rejoinToken,
      hostConnected: res.hostConnected,
      roomExpiresInSeconds: res.roomExpiresInSeconds,
      rejoinTokenExpiresAt: res.rejoinTokenExpiresAt,
      state: this.snapshotState(res),
    };
  }

  /**
   * Recovery entry point: reattach to a slot using a previously persisted
   * {@link RoomSession} (e.g. after a page reload). Always uses a fresh epoch,
   * since a reloaded page has lost its WebRTC session.
   */
  async rejoin(session: RoomSession): Promise<RejoinResult> {
    const epoch = generateEpoch();
    // Capture the previous other-epoch BEFORE applyHandshakeResponse overwrites
    // the persisted session, so the epoch-change comparison is correct.
    const prevOtherEpoch = this.otherEpochOf(session.role, session);
    const message: ClientMessage = { type: 'rejoin-room', rejoinToken: session.rejoinToken, epoch };
    const res = (await this.handshake('rejoin', message)) as RejoinRoomResponse;
    this.applyHandshakeResponse(res, {
      myEpoch: epoch,
      preloadedRole: session.role,
      rejoinToken: session.rejoinToken,
    });
    this.handleRejoinResponse(res, prevOtherEpoch);
    return {
      recreated: res.recreated,
      peerConnected: res.peerConnected,
      state: this.snapshotState(res),
    };
  }

  // -------------------------------------------------------------------------
  // Media API
  //
  // These methods accept calls before room creation, while waiting for a peer,
  // during signaling, and after connection. Callers never need to know whether
  // an internal `SimplePeer` currently exists. Tracks registered here are
  // retained in a desired-media registry and re-attached to every new peer
  // generation (initial construction, peer-reset rebuild, rejoin rebuild).
  // -------------------------------------------------------------------------

  /**
   * Add a `MediaStreamTrack` to the connection. The track is recorded in the
   * desired-media registry and attached to the current internal peer (if any)
   * and to every future peer generation. Safe to call before `createRoom` /
   * `joinRoom`, while waiting for `guest-joined` (host), or after `connect`.
   *
   * The wrapper does not take ownership of the track or stop it on close; the
   * application owns the track's lifecycle. Ended tracks are ignored on
   * re-attachment and may be removed with {@link removeTrack}.
   *
   * Deduplication is by track id: calling `addTrack` twice with the same track
   * (and any stream) is a no-op after the first call.
   */
  addTrack(track: MediaStreamTrack, stream: MediaStream): void {
    if (!track || !stream) return;
    if (this.desiredTracks.has(track.id)) {
      // Already registered. Still attempt to attach to the current peer in
      // case the previous attach failed (e.g. peer was destroyed in between).
      this.attachTrackToCurrentPeer(track, stream, track.id);
      return;
    }
    this.desiredTracks.set(track.id, { track, stream, attachedGenerations: new Set() });
    this.attachTrackToCurrentPeer(track, stream, track.id);
  }

  /**
   * Remove a `MediaStreamTrack` from the connection. Detaches it from the
   * current peer (if any) and removes it from the desired-media registry so it
   * is not re-attached to a future peer generation. Does not stop the track.
   */
  removeTrack(track: MediaStreamTrack, stream: MediaStream): void {
    if (!track) return;
    this.desiredTracks.delete(track.id);
    const peer = this.peer;
    if (!peer) return;
    try {
      peer.removeTrack(track, stream);
    } catch (e) {
      this.emitMediaError('removeTrack failed', e);
    }
  }

  /**
   * Replace one track with another on the current peer and in the desired-media
   * registry. The new track is registered in place of the old one (so it is
   * re-attached to future peer generations); the old track is removed from the
   * registry but not stopped.
   *
   * Returns the promise from the underlying `replaceTrack`. If no peer
   * currently exists, the registry is updated synchronously and `undefined` is
   * returned; the new track will be attached when the next peer is built.
   */
  replaceTrack(
    oldTrack: MediaStreamTrack,
    newTrack: MediaStreamTrack,
    stream: MediaStream,
  ): Promise<void> | void {
    if (!oldTrack || !newTrack || !stream) return;
    // Update the registry: drop the old track, add the new one (preserving
    // any attachment history is not meaningful since the new track is a
    // different object).
    this.desiredTracks.delete(oldTrack.id);
    if (!this.desiredTracks.has(newTrack.id)) {
      this.desiredTracks.set(newTrack.id, { track: newTrack, stream, attachedGenerations: new Set() });
    }
    const peer = this.peer;
    if (!peer) {
      // No peer yet — the new track will be attached when the next peer is built.
      return;
    }
    try {
      return peer.replaceTrack(oldTrack, newTrack, stream).catch((e: unknown) => {
        this.emitMediaError('replaceTrack rejected', e);
        throw e;
      });
    } catch (e) {
      // A synchronous throw (bad arguments, destroyed peer) rather than a
      // rejected promise.
      this.emitMediaError('replaceTrack threw', e);
      throw e;
    }
  }

  /**
   * Add every track of `stream` to the connection (convenience wrapper around
   * {@link addTrack}). The stream itself is not retained by the wrapper; only
   * its individual tracks are registered. Use {@link setLocalStream} for a
   * higher-level API that retains the stream reference.
   */
  addStream(stream: MediaStream): void {
    if (!stream) return;
    for (const t of stream.getTracks()) this.addTrack(t, stream);
  }

  /**
   * Remove every track of `stream` from the connection (convenience wrapper
   * around {@link removeTrack}). Does not stop the tracks.
   */
  removeStream(stream: MediaStream): void {
    if (!stream) return;
    for (const t of stream.getTracks()) this.removeTrack(t, stream);
  }

  /**
   * Select the local `MediaStream` to send to the remote peer. This is the
   * recommended high-level API for voice/video: the wrapper retains the
   * stream reference and ensures its tracks are attached to every internal
   * `SimplePeer` generation (initial construction and rebuilds).
   *
   * Semantics:
   *
   * - May be called before or after `createRoom` / `joinRoom`.
   * - Calling with a new stream replaces the previously selected one: tracks
   *   that are no longer present are removed from the registry (and detached
   *   from the current peer), and new tracks are added.
   * - Calling with `null` stops sending. Tracks previously registered via
   *   `setLocalStream` are removed from the registry and detached; the
   *   application retains ownership of the tracks and may stop them.
   * - Tracks added directly via {@link addTrack} are not affected by this call
   *   unless they belong to the previously selected stream.
   * - The selected stream survives internal peer reconstruction.
   *
   * The wrapper does not stop tracks owned by the caller.
   */
  setLocalStream(stream: MediaStream | null): void {
    const prev = this.localStream;
    this.localStream = stream;
    if (prev && prev !== stream) {
      // Remove tracks that belonged to the previous stream (only those still
      // registered as desired tracks; tracks the caller already removed are
      // gone from the registry).
      for (const t of prev.getTracks()) {
        const entry = this.desiredTracks.get(t.id);
        if (!entry) continue;
        // Only remove if the registry still points at the prev stream.
        if (entry.stream === prev) this.removeTrack(t, prev);
      }
    }
    if (stream) {
      for (const t of stream.getTracks()) this.addTrack(t, stream);
    }
  }

  /**
   * Snapshot of media-related state for diagnostics. Intentionally avoids
   * exposing SDP, device labels, IP addresses, or other sensitive details.
   */
  get mediaDiagnostics(): MediaDiagnostics {
    const desired = [...this.desiredTracks.values()].map((e) => ({
      kind: e.track.kind,
      readyState: e.track.readyState,
      id: e.track.id,
    }));
    return {
      peerGeneration: this.peerGenerationField,
      hasPeer: this.peer !== null,
      peerConnected: this.peer?.connected ?? false,
      desiredTrackCount: this.desiredTracks.size,
      desiredTracks: desired,
      hasLocalStream: this.localStream !== null,
    };
  }

  /**
   * Passthrough to the underlying `RTCPeerConnection.getStats()` when an
   * internal peer exists. Returns `null` when no peer is currently
   * constructed. Useful for connection-quality monitoring; consumers should
   * avoid logging SDP, candidate IPs, or device labels from the report.
   */
  async getStats(): Promise<RTCStatsReport | null> {
    const peer = this.peer;
    if (!peer) return null;
    return peer.getStats();
  }

  // -------------------------------------------------------------------------
  // Data channel API
  //
  // Like the media API, these accept calls before room creation, while waiting
  // for a peer, and after connection. Handles are created eagerly and rebound
  // to each new peer generation, so callers never need to know whether an
  // internal peer currently exists.
  // -------------------------------------------------------------------------

  /**
   * The stable handle for a declared or dynamic data channel.
   *
   * Callable before `createRoom` / `joinRoom`, in which case the handle reports
   * `'connecting'` until the peer is built and the channel opens. The returned
   * object keeps its identity for the lifetime of this connection, including
   * across peer rebuilds, so it is safe to retain and to attach listeners to
   * exactly once.
   *
   * @throws {Error} for a label that was neither declared in `dataChannels` nor
   *   opened via {@link openChannel}.
   */
  channel(label: string): DataChannelHandle {
    assertUsableLabel(label);
    const handle = this.channelHandles.get(label);
    if (!handle) {
      throw new Error(
        `Unknown data channel "${label}". Declare it in options.dataChannels or call openChannel().`,
      );
    }
    return handle;
  }

  /**
   * Open a data channel that was not declared at construction.
   *
   * Safe from either side. If no peer exists yet the handle is registered and
   * the channel is created when the next peer is built, so this may be called
   * at any point in the lifecycle.
   */
  openChannel(label: string, spec: DataChannelSpec = {}): DataChannelHandle {
    assertUsableLabel(label);
    // Join the declared set so the channel is recreated on every subsequent
    // peer generation. Without this a channel opened at runtime would vanish at
    // the next epoch change, leaving the application holding a handle that
    // never reopens.
    this.declaredChannels[label] = spec;

    const existing = this.channelHandles.get(label);
    if (existing) {
      // Already known: make sure the current peer has actually created it.
      if (this.peer) this.peer.openChannel(label, spec);
      return existing;
    }
    if (this.peer) return this.peer.openChannel(label, spec);
    // No peer yet — register the handle now; the next generation creates the
    // underlying channel from the declared set.
    const handle = new DataChannelHandle(label, resolveSpec(spec));
    this.channelHandles.set(label, handle);
    return handle;
  }

  /** Every known data channel handle, keyed by label. */
  get channels(): ReadonlyMap<string, DataChannelHandle> {
    return new Map(this.channelHandles);
  }

  /**
   * Per-channel diagnostics for the current peer generation. Carries no message
   * contents. Empty when no peer exists.
   */
  get dataChannelDiagnostics(): ChannelDiagnostics[] {
    return this.peer?.channelDiagnostics ?? [];
  }

  /**
   * Gracefully end the session. Sends `close-room`, destroys the peer, and
   * closes the transport. Suppresses any reconnect attempt.
   *
   * Media notes: retained desired-track references are cleared so they can be
   * garbage-collected once the application drops its own references. The
   * wrapper does not call `track.stop()` on any track; the application owns
   * track lifecycle.
   */
  close(reason = 'closed-by-client'): void {
    if (this.userClosed) return;
    this.userClosed = true;
    this.clearReconnectTimer();
    this.log.info?.('close', { reason });
    if (this.transport && !this.socketReleased) {
      try {
        this.transport.send({ type: 'close-room' });
      } catch {
        /* socket already gone */
      }
    }
    this.destroyPeer('user-close');
    this.desiredTracks.clear();
    this.localStream = null;
    this.disposeChannels();
    this.transport.close(CloseCodeEnum.ROOM_CLOSED, reason);
    this.setStatus('closed');
    this.emitClose(reason);
  }

  /** Hard cleanup without sending `close-room`. Use on fatal local errors. */
  destroy(): void {
    this.userClosed = true;
    this.clearReconnectTimer();
    this.destroyPeer('user-close');
    this.desiredTracks.clear();
    this.localStream = null;
    this.disposeChannels();
    this.transport.close();
  }

  /**
   * Retire every channel handle. Called only from `close`/`destroy` — a peer
   * rebuild must leave handles alive, since the application still holds them.
   */
  private disposeChannels(): void {
    for (const handle of this.channelHandles.values()) handle.dispose();
    this.channelHandles.clear();
  }

  // -------------------------------------------------------------------------
  // Transport wiring
  // -------------------------------------------------------------------------

  private wireTransport(): void {
    this.transport.onMessage((m) => {
      if (m) this.handleServerMessage(m);
      else this.log.warn?.('received unparseable frame');
    });
    this.transport.onClose((e) => this.handleTransportClose(e.code, e.reason));
    this.transport.onError((err) => {
      this.log.error?.('transport error', { message: err.message });
    });
  }

  private async handshake(kind: HandshakeKind, message: ClientMessage): Promise<ServerMessage> {
    if (this.terminal) throw new SignalingError(0, 'PeerConnection is terminal', { retryable: false });
    this.setStatus('connecting');
    if (!this.transportOpen()) {
      await this.transport.connect(this.opts.url);
    }
    const requestId = generateRequestId();
    const toSend: ClientMessage = { ...message, requestId };
    return new Promise<ServerMessage>((resolve, reject) => {
      this.pendingHandshake = {
        kind,
        requestId,
        message: toSend,
        resolve,
        reject,
      };
      try {
        this.transport.send(toSend);
      } catch (e) {
        this.pendingHandshake = null;
        reject(new SignalingError(0, `Failed to send ${kind} handshake`, { cause: e, retryable: true }));
      }
    });
  }

  private transportOpen(): boolean {
    // readyState 1 = OPEN for the standard WebSocket interface.
    return (this.transport as unknown as { socket?: WebSocketLike }).socket?.readyState === 1;
  }

  // -------------------------------------------------------------------------
  // Server message dispatch
  // -------------------------------------------------------------------------

  private handleServerMessage(msg: ServerMessage): void {
    switch (msg.type) {
      case 'create-room-response':
      case 'join-room-response':
      case 'rejoin-room-response':
        this.resolveHandshake(msg);
        break;
      case 'error-response':
        this.handleErrorResponse(msg);
        break;
      case 'signal-response':
        this.handleSignalResponse(msg);
        break;
      case 'guest-joined':
        this.handleGuestJoined(msg);
        break;
      case 'peer-disconnected':
        this.emit('peer-disconnected', { role: msg.role });
        this.log.info?.('peer-disconnected', { role: msg.role });
        break;
      case 'peer-rejoined':
        this.emit('peer-rejoined', { role: msg.role, epoch: msg.epoch });
        this.log.info?.('peer-rejoined', { role: msg.role, epoch: msg.epoch });
        break;
      case 'peer-reset':
        this.handlePeerReset(msg);
        break;
      case 'room-idle-close':
        this.handleRoomIdleClose(msg);
        break;
      case 'room-closed':
        this.handleRoomClosed(msg);
        break;
      case 'room-expired':
        this.handleRoomExpired();
        break;
      case 'server-shutdown':
        this.handleServerShutdown(msg);
        break;
      default:
        this.log.warn?.('unknown server message', { type: (msg as { type: string }).type });
    }
  }

  private resolveHandshake(msg: ServerMessage): void {
    const pending = this.pendingHandshake;
    if (!pending) return;
    // An error-response with the same requestId is handled by handleErrorResponse.
    this.pendingHandshake = null;
    this.reconnectAttempt = 0;
    pending.resolve(msg);
  }

  private handleErrorResponse(msg: ErrorResponseMessage): void {
    const err = SignalingError.fromErrorResponse(msg);
    const pending = this.pendingHandshake;
    if (pending && msg.requestId === pending.requestId) {
      this.pendingHandshake = null;
      pending.reject(err);
      return;
    }
    // Non-handshake error.
    if (err.code === 1303 /* SIGNAL_BUFFER_OVERFLOW */) {
      this.log.warn?.('signal buffer overflow; rebuilding peer with new epoch');
      this.rebuildPeerWithNewEpoch();
      this.emit('error', err);
      return;
    }
    this.emit('error', err);
    if (!err.retryable) {
      this.log.error?.('non-retryable error', { code: err.code, message: err.message });
    }
  }

  // -------------------------------------------------------------------------
  // Handshake response application
  // -------------------------------------------------------------------------

  private applyHandshakeResponse(
    res: CreateRoomResponse | JoinRoomResponse | RejoinRoomResponse,
    ctx: { myEpoch: string; preloadedRole?: Role; rejoinToken?: string },
  ): void {
    const role: Role =
      res.type === 'create-room-response'
        ? 'host'
        : res.type === 'join-room-response'
          ? 'guest'
          : (ctx.preloadedRole ?? res.role);
    const roomId = res.roomId;
    const hostEpoch = res.type === 'create-room-response' ? ctx.myEpoch : res.hostEpoch;
    const guestEpoch =
      res.type === 'create-room-response'
        ? null
        : res.type === 'join-room-response'
          ? ctx.myEpoch
          : res.guestEpoch;

    const peerDeadlineAt: string | null =
      res.type === 'create-room-response'
        ? res.peerDeadlineAt
        : res.type === 'rejoin-room-response'
          ? res.peerDeadlineAt
          : null; // join-room-response: joining clears the peer deadline.

    // rejoin-room-response does not carry a fresh rejoinToken — the client
    // already holds the one it used to rejoin. Preserve it.
    const rejoinToken =
      res.type === 'rejoin-room-response'
        ? (ctx.rejoinToken ?? this.state?.rejoinToken ?? '')
        : res.rejoinToken;

    const state: RoomState = {
      roomId,
      role,
      rejoinToken,
      hostEpoch: hostEpoch ?? null,
      guestEpoch: guestEpoch ?? null,
      peerDeadlineAt,
      roomExpiresAt: res.roomExpiresAt,
      rejoinTokenExpiresAt: res.rejoinTokenExpiresAt,
    };
    this.state = state;
    this.persistSession();
    this.emit('state', { ...state });
  }

  private handleRejoinResponse(res: RejoinRoomResponse, prevOtherEpoch: string | null): void {
    // Compare the epoch for the OTHER role against what we had stored. The
    // caller captures `prevOtherEpoch` BEFORE applyHandshakeResponse overwrites
    // the persisted session, otherwise the comparison would always be equal.
    const otherRole: Role = this.state?.role === 'host' ? 'guest' : 'host';
    const newOtherEpoch = otherRole === 'host' ? res.hostEpoch : res.guestEpoch;

    const rebuilt = this.maybeBuildPeer({
      initiator: this.state?.role === 'host',
    });

    if (res.recreated) {
      // The peer's WebRTC session is definitively gone; we already built a
      // fresh SimplePeer above (with a fresh seq counter).
      this.log.info?.('room recreated from token');
      this.setStatus(this.state?.role === 'host' ? 'waiting-for-peer' : 'signaling');
      return;
    }

    if (prevOtherEpoch !== null && prevOtherEpoch !== newOtherEpoch) {
      // The peer's epoch changed — destroy & rebuild (we already rebuilt above
      // if there was no peer; if there was one, rebuild now).
      if (!rebuilt) this.rebuildPeerWithNewEpoch();
      this.emit('peer-reset', { role: otherRole, epoch: newOtherEpoch ?? '' });
    }

    if (res.peerConnected) {
      this.setStatus(this.peer?.connected ? 'connected' : 'signaling');
    } else {
      this.setStatus('waiting-for-peer');
    }
  }

  /** The epoch for the role that is NOT `role`, read from a snapshot. */
  private otherEpochOf(
    role: Role,
    snap: { hostEpoch: string | null; guestEpoch: string | null } | null,
  ): string | null {
    if (!snap) return null;
    const otherRole: Role = role === 'host' ? 'guest' : 'host';
    return otherRole === 'host' ? snap.hostEpoch : snap.guestEpoch;
  }

  // -------------------------------------------------------------------------
  // SimplePeer lifecycle
  // -------------------------------------------------------------------------

  /**
   * Build a SimplePeer if one does not already exist. Returns true if a new
   * peer was constructed. `initiator` is derived from our role when not given.
   *
   * After construction, all live desired tracks are attached to the new peer
   * (so tracks registered before `guest-joined` on the host, or before
   * `joinRoom` resolves on the guest, are included in the initial offer), and
   * a `peer-created` event is fired with the new generation number.
   */
  private maybeBuildPeer(opts: { initiator: boolean }): boolean {
    if (this.peer) return false;
    const initiator = opts.initiator;
    const peer = this.createPeer({
      ...this.rtcOpts,
      initiator,
      dataChannels: this.declaredChannels,
      // The same handle map for every generation, so application references
      // stay valid across rebuilds.
      channelHandles: this.channelHandles,
      ...(this.rtcOpts.logger === undefined ? { logger: this.log } : {}),
    });
    this.wirePeer(peer);
    this.peer = peer;
    this.peerGenerationField += 1;
    const generation = this.peerGenerationField;
    this.seq.reset();
    this.log.info?.('constructed peer', { initiator, generation });
    // Attach desired media BEFORE firing peer-created so that handlers see the
    // tracks already included in the initial offer/answer.
    this.attachDesiredTracksToPeer(peer, generation);
    this.emit('peer-created', { peer, generation });
    if (!this.socketReleased) this.setStatus('signaling');
    return true;
  }

  private wirePeer(peer: RtcPeerLike): void {
    peer.on('signal', (data) => this.sendSignal(data));
    peer.on('connect', () => {
      this.log.info?.('peer connected');
      this.setStatus('connected');
      this.sendPeerConnected();
      this.emit('connect', undefined);
    });
    peer.on('data', (chunk) => this.emit('data', chunk));
    peer.on('stream', (stream) => this.emit('stream', stream));
    peer.on('track', ({ track, stream }) => this.emit('track', { track, stream }));
    peer.on('channel-open', (e) => this.emit('channel-open', e));
    peer.on('channel-close', (e) => this.emit('channel-close', e));
    peer.on('channel-message', (e) => this.emit('channel-message', e));
    peer.on('channel-error', (e) => this.emit('channel-error', e));
    peer.on('channel', (e) => this.emit('channel', e));
    peer.on('close', () => {
      this.log.info?.('peer close');
      const generation = this.peerGenerationField;
      this.peer = null;
      this.emit('peer-destroyed', { generation, reason: 'close' });
      if (!this.userClosed && !this.terminal) {
        // The P2P connection died. If the socket is still attached, the server
        // is still around — rejoin to re-signal. If the socket was released,
        // we need a fresh rejoin to recreate the room.
        this.triggerReconnect(new SignalingError(0, 'peer closed', { retryable: true }));
      }
    });
    peer.on('error', (err) => {
      this.log.error?.('peer error', { message: err.message });
      // The engine always emits `close` after `error`, which drives reconnect.
    });
  }

  private destroyPeer(reason: PeerDestroyedReason = 'rebuild'): void {
    if (!this.peer) return;
    const p = this.peer;
    const generation = this.peerGenerationField;
    this.peer = null;
    try {
      p.destroy();
    } catch {
      /* ignore */
    }
    this.emit('peer-destroyed', { generation, reason });
  }

  /** Destroy the current SimplePeer and build a fresh one with a new epoch. */
  private rebuildPeerWithNewEpoch(): void {
    this.destroyPeer('rebuild');
    // Note: we do NOT change our own epoch here. A peer-reset from the server
    // means the *other* side changed epoch; our epoch stays the same and our
    // seq counter resets only because we built a new peer.
    this.maybeBuildPeer({ initiator: this.state?.role === 'host' });
  }

  // -------------------------------------------------------------------------
  // Desired-media registry helpers
  // -------------------------------------------------------------------------

  /**
   * Attach every live desired track to a freshly constructed peer. Tracks
   * whose `readyState` is `'ended'` are skipped (and pruned from the
   * registry). Each track is attached at most once per generation via the
   * `attachedGenerations` set on its entry.
   */
  private attachDesiredTracksToPeer(peer: RtcPeerLike, generation: number): void {
    for (const [id, entry] of this.desiredTracks) {
      if (entry.track.readyState === 'ended') {
        this.desiredTracks.delete(id);
        continue;
      }
      if (entry.attachedGenerations.has(generation)) continue;
      try {
        peer.addTrack(entry.track, entry.stream);
        entry.attachedGenerations.add(generation);
      } catch (e) {
        this.emitMediaError(`addTrack failed for track ${id}`, e);
      }
    }
  }

  /**
   * Attach a single track to the current peer (if any), respecting the
   * per-generation dedup set on its registry entry.
   */
  private attachTrackToCurrentPeer(
    track: MediaStreamTrack,
    stream: MediaStream,
    id: string,
  ): void {
    if (track.readyState === 'ended') return;
    const peer = this.peer;
    if (!peer) return;
    const entry = this.desiredTracks.get(id);
    if (!entry) return;
    const generation = this.peerGenerationField;
    if (entry.attachedGenerations.has(generation)) return;
    try {
      peer.addTrack(track, stream);
      entry.attachedGenerations.add(generation);
    } catch (e) {
      this.emitMediaError(`addTrack failed for track ${id}`, e);
    }
  }

  private emitMediaError(message: string, cause?: unknown): void {
    this.log.warn?.('media-error', { message, cause: (cause as Error)?.message });
    this.emit('media-error', { message, cause });
  }

  // -------------------------------------------------------------------------
  // Signaling relay
  // -------------------------------------------------------------------------

  private sendSignal(data: SignalData): void {
    const payload = JSON.stringify(data);
    if (this.socketReleased && this.peer?.connected) {
      // After the server released the sockets, carry renegotiation peer-to-peer
      // over the engine's dedicated control channel. See DESIGN.md
      // "Renegotiation after socket release".
      if (!this.peer.sendControlSignal(data)) {
        this.log.error?.('failed to send control signal: channel not open');
      }
      return;
    }
    if (!this.transportOpen()) {
      this.log.warn?.('dropping signal: transport not open');
      return;
    }
    const seq = this.seq.next();
    const msg: ClientMessage = { type: 'signal', seq, data: payload };
    try {
      this.transport.send(msg);
    } catch (e) {
      this.log.error?.('failed to send signal', { seq, message: (e as Error).message });
    }
  }

  private handleSignalResponse(msg: SignalResponseMessage): void {
    if (!this.peer) {
      this.log.warn?.('signal-response with no peer', { seq: msg.seq });
      return;
    }
    let data: unknown;
    try {
      data = JSON.parse(msg.data);
    } catch {
      this.log.warn?.('could not parse signal data', { seq: msg.seq });
      return;
    }
    try {
      this.peer.signal(data);
    } catch (e) {
      this.log.warn?.('peer.signal threw', { seq: msg.seq, message: (e as Error).message });
    }
  }

  private sendPeerConnected(): void {
    if (!this.transportOpen() || this.socketReleased) return;
    try {
      this.transport.send({ type: 'peer-connected' });
    } catch (e) {
      this.log.warn?.('failed to send peer-connected', { message: (e as Error).message });
    }
  }

  // -------------------------------------------------------------------------
  // Server-initiated events
  // -------------------------------------------------------------------------

  private handleGuestJoined(msg: { guestEpoch: string }): void {
    if (this.state?.role !== 'host') {
      this.log.warn?.('guest-joined received but role is not host');
      return;
    }
    // Record the guest epoch and rebuild if it changed (e.g. server restarted
    // and guest reconnected first with a new epoch).
    const prevGuestEpoch = this.state.guestEpoch;
    this.state = { ...this.state, guestEpoch: msg.guestEpoch, peerDeadlineAt: null };
    this.persistSession();
    this.emit('state', { ...this.state });
    this.emit('guest-joined', { guestEpoch: msg.guestEpoch });
    // Host builds its SimplePeer (as initiator) now.
    if (prevGuestEpoch !== null && prevGuestEpoch !== msg.guestEpoch && this.peer) {
      this.rebuildPeerWithNewEpoch();
    } else {
      this.maybeBuildPeer({ initiator: true });
    }
  }

  private handlePeerReset(msg: { role: Role; epoch: string }): void {
    // The other side reattached with a new epoch. Destroy & rebuild our peer.
    const otherRole = msg.role;
    if (this.state) {
      const next = { ...this.state };
      if (otherRole === 'host') next.hostEpoch = msg.epoch;
      else next.guestEpoch = msg.epoch;
      this.state = next;
      this.persistSession();
      this.emit('state', { ...this.state });
    }
    this.rebuildPeerWithNewEpoch();
    this.emit('peer-reset', { role: otherRole, epoch: msg.epoch });
  }

  private handleRoomIdleClose(msg: { reason: 'peer-connected' }): void {
    this.socketReleased = true;
    this.log.info?.('room-idle-close', { reason: msg.reason });
    this.emit('socket-released', undefined);
    // The server will follow with close 4200; handleTransportClose treats that
    // as success and does not reconnect.
  }

  private handleRoomClosed(msg: { reason: string }): void {
    this.terminal = true;
    this.log.info?.('room-closed', { reason: msg.reason });
    this.emit('room-closed', { reason: msg.reason });
    if (this.state) this.sessionStore.delete(this.state.roomId);
  }

  private handleRoomExpired(): void {
    this.terminal = true;
    this.log.info?.('room-expired');
    this.emit('room-expired', undefined);
    if (this.state) this.sessionStore.delete(this.state.roomId);
  }

  private handleServerShutdown(msg: { reconnectAfterMs: number }): void {
    this.log.info?.('server-shutdown', { reconnectAfterMs: msg.reconnectAfterMs });
    this.emit('server-shutdown', { reconnectAfterMs: msg.reconnectAfterMs });
  }

  // -------------------------------------------------------------------------
  // Reconnect logic
  // -------------------------------------------------------------------------

  private handleTransportClose(code: number, reason: string): void {
    if (this.userClosed) return;
    // 4200 = released after peer connection — success, do not reconnect.
    if (code === CloseCodeEnum.RELEASED_AFTER_PEER_CONNECTED) {
      this.socketReleased = true;
      this.emit('socket-released', undefined);
      return;
    }
    // 4400 = replaced by a newer connection — terminal.
    if (code === CloseCodeEnum.REPLACED) {
      this.terminal = true;
      const err = new SignalingError(code, `Replaced by a newer connection: ${reason}`, {
        retryable: false,
      });
      this.failTerminal(err, `replaced: ${reason}`);
      return;
    }
    // 4013 = room closed — terminal.
    if (code === CloseCodeEnum.ROOM_CLOSED) {
      this.terminal = true;
      this.emit('room-closed', { reason });
      if (this.state) this.sessionStore.delete(this.state.roomId);
      this.failTerminal(new SignalingError(code, `Room closed: ${reason}`), `room-closed: ${reason}`);
      return;
    }
    // No state to rejoin with — nothing to do but surface.
    if (!this.state) {
      this.failTerminal(
        new SignalingError(code, `Socket closed before handshake: ${reason}`),
        `closed: ${reason}`,
      );
      return;
    }
    // Reconnectable close — schedule a rejoin with a fresh epoch.
    const err = SignalingError.fromCloseCode(code, reason);
    this.triggerReconnect(err);
  }

  private triggerReconnect(lastError: SignalingError): void {
    if (this.userClosed || this.terminal) return;
    if (!lastError.retryable) {
      this.failTerminal(lastError, lastError.message);
      return;
    }
    if (this.reconnectAttempt >= this.opts.maxReconnectAttempts) {
      this.failTerminal(lastError, 'max reconnect attempts exceeded');
      return;
    }
    // Check token expiry before bothering to reconnect.
    if (this.state && this.isTokenExpired()) {
      this.terminal = true;
      this.emit('room-expired', undefined);
      if (this.state) this.sessionStore.delete(this.state.roomId);
      this.failTerminal(new SignalingError(0, 'rejoin token expired'), 'token-expired');
      return;
    }
    const delay =
      lastError.retryAfterMs ??
      fullJitterBackoff(this.reconnectAttempt);
    this.reconnectAttempt += 1;
    this.setStatus('reconnecting');
    this.log.info?.('reconnect scheduled', { delay, attempt: this.reconnectAttempt });
    this.clearReconnectTimer();
    this.reconnectTimer = setTimeout(() => {
      void this.doRejoin();
    }, delay);
  }

  private async doRejoin(): Promise<void> {
    if (this.userClosed || this.terminal || !this.state) return;
    // Always use a fresh epoch on reconnect: the WebRTC session is presumed gone.
    const epoch = generateEpoch();
    // Snapshot the previous other-epoch before applyHandshakeResponse overwrites it.
    const prevOtherEpoch = this.otherEpochOf(this.state.role, this.state);
    const message: ClientMessage = {
      type: 'rejoin-room',
      rejoinToken: this.state.rejoinToken,
      epoch,
    };
    try {
      const res = (await this.handshake('rejoin', message)) as RejoinRoomResponse;
      this.applyHandshakeResponse(res, {
        myEpoch: epoch,
        preloadedRole: this.state.role,
        rejoinToken: this.state.rejoinToken,
      });
      this.handleRejoinResponse(res, prevOtherEpoch);
    } catch (e) {
      const err = e as SignalingError;
      this.log.warn?.('rejoin failed', { code: err.code, message: err.message });
      if (err.code === 1203 /* INVALID_REJOIN_TOKEN */) {
        if (this.state) this.sessionStore.delete(this.state.roomId);
        this.failTerminal(err, 'invalid rejoin token');
        return;
      }
      if (err.code === 1104 /* ROOM_CLOSED */ || err.code === 1105 /* ROOM_EXPIRED */) {
        if (this.state) this.sessionStore.delete(this.state.roomId);
        this.failTerminal(err, err.message);
        return;
      }
      if (!err.retryable) {
        this.failTerminal(err, err.message);
        return;
      }
      // Retryable — schedule another attempt.
      this.triggerReconnect(err);
    }
  }

  private isTokenExpired(): boolean {
    if (!this.state?.rejoinTokenExpiresAt) return false;
    const t = Date.parse(this.state.rejoinTokenExpiresAt);
    if (Number.isNaN(t)) return false;
    return Date.now() >= t;
  }

  private failTerminal(err: SignalingError, reason: string): void {
    this.terminal = true;
    this.clearReconnectTimer();
    this.destroyPeer('terminal');
    this.log.error?.('terminal', { reason, code: err.code, message: err.message });
    this.emit('error', err);
    this.setStatus('error');
    this.emitClose(reason);
  }

  // -------------------------------------------------------------------------
  // Helpers
  // -------------------------------------------------------------------------

  private persistSession(): void {
    if (!this.state) return;
    const session: RoomSession = {
      roomId: this.state.roomId,
      role: this.state.role,
      rejoinToken: this.state.rejoinToken,
      hostEpoch: this.state.hostEpoch,
      guestEpoch: this.state.guestEpoch,
      ...(this.state.rejoinTokenExpiresAt !== undefined
        ? { rejoinTokenExpiresAt: this.state.rejoinTokenExpiresAt }
        : {}),
    };
    this.sessionStore.save(session);
  }

  private snapshotState(
    res: CreateRoomResponse | JoinRoomResponse | RejoinRoomResponse,
  ): RoomState {
    // applyHandshakeResponse already set this.state from the same response.
    void res;
    return this.state ? { ...this.state } : ({} as RoomState);
  }

  private setStatus(status: PeerStatus): void {
    if (this.status === status) return;
    this.status = status;
    this.emit('status', status);
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private emitClose(reason: string): void {
    this.emit('close', { reason });
  }
}
