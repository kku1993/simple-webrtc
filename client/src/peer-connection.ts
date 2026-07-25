import SimplePeer from 'simple-peer';
import {
  type ClientMessage,
  type CreateRoomResponse,
  type DataChannelFrame,
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
import { type RoomSession, type SessionStore, RoomSessionStore, MemorySessionStore } from './storage.js';
import { Transport, type WebSocketFactory, type WebSocketLike } from './transport.js';
import { SequenceCounter, generateEpoch, generateRequestId, fullJitterBackoff } from './util.js';

export { SignalingError } from './errors.js';
export * from './types.js';
export * from './storage.js';
export type { WebSocketFactory, WebSocketLike } from './transport.js';

// ---------------------------------------------------------------------------
// Public option / result types
// ---------------------------------------------------------------------------

/** Options forwarded to `new SimplePeer(...)` except `initiator`, which is owned by this class. */
export type SimplePeerOptions = Omit<SimplePeer.Options, 'initiator'>;

/**
 * The slice of `simple-peer`'s instance API that {@link PeerConnection} relies
 * on. Declared locally so tests can inject a stub without depending on `wrtc`,
 * and so the type-checked surface stays small.
 */
export interface PeerLike {
  readonly connected: boolean;
  on(event: 'signal', fn: (data: SimplePeer.SignalData) => void): this;
  on(event: 'connect', fn: () => void): this;
  on(event: 'data', fn: (chunk: unknown) => void): this;
  on(event: 'stream', fn: (stream: MediaStream) => void): this;
  on(event: 'track', fn: (track: MediaStreamTrack, stream: MediaStream) => void): this;
  on(event: 'close', fn: () => void): this;
  on(event: 'error', fn: (err: Error) => void): this;
  signal(data: string | SimplePeer.SignalData): void;
  send(data: unknown): void;
  destroy(error?: Error): unknown;
}

/** Factory used to construct each `SimplePeer`. Overridable for tests. */
export type SimplePeerFactory = (opts: SimplePeerOptions & { initiator: boolean }) => PeerLike;

export interface Logger {
  debug?(msg: string, ctx?: Record<string, unknown>): void;
  info?(msg: string, ctx?: Record<string, unknown>): void;
  warn?(msg: string, ctx?: Record<string, unknown>): void;
  error?(msg: string, ctx?: Record<string, unknown>): void;
}

export interface PeerConnectionOptions {
  /** WebSocket URL of the signaling endpoint, e.g. `wss://host/v1/signal`. */
  url: string;
  /** Options forwarded to every `new SimplePeer(...)` (config, streams, …). */
  simplePeer?: SimplePeerOptions;
  /** Persistence for `{ roomId, role, rejoinToken, epochs }`. Defaults to in-memory. */
  store?: SessionStore;
  /** Override the WebSocket constructor (mainly for tests). */
  transportFactory?: WebSocketFactory;
  /** Override the `SimplePeer` constructor (mainly for tests). */
  simplePeerFactory?: SimplePeerFactory;
  /** Structured logger; defaults to no-op. */
  logger?: Logger;
  /**
   * Maximum reconnect attempts before surfacing a terminal error. Defaults to
   * `Infinity` — the client keeps trying until the rejoin token expires or the
   * user closes it.
   */
  maxReconnectAttempts?: number;
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

export interface PeerConnectionEvents {
  /** Room state changed (epochs, deadlines, role). */
  state: RoomState;
  /** High-level status transition. */
  status: PeerStatus;
  /** Host only: the guest joined. The SimplePeer is being constructed now. */
  'guest-joined': { guestEpoch: string };
  /** The underlying simple-peer connected (P2P established). */
  connect: void;
  /** Data received over the P2P data channel. */
  data: unknown;
  /** A remote media stream was added. */
  stream: MediaStream;
  /** A remote track was added. */
  track: { track: MediaStreamTrack; stream: MediaStream };
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
  /** The peer connection has fully closed. */
  close: { reason: string };
}

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
  private readonly opts: Required<Omit<PeerConnectionOptions, 'simplePeer' | 'store' | 'transportFactory' | 'simplePeerFactory' | 'logger'>>;
  private readonly simplePeerOpts: SimplePeerOptions;
  private readonly sessionStore: RoomSessionStore;
  private readonly transport: Transport;
  private readonly log: Logger;
  private readonly createPeer: SimplePeerFactory;

  /** Current room state, or null before a successful handshake. */
  private state: RoomState | null = null;
  /** The underlying simple-peer instance, when one exists. */
  private peer: PeerLike | null = null;
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

  constructor(opts: PeerConnectionOptions) {
    super();
    this.opts = {
      url: opts.url,
      maxReconnectAttempts: opts.maxReconnectAttempts ?? Infinity,
    };
    this.simplePeerOpts = opts.simplePeer ?? {};
    this.sessionStore = new RoomSessionStore(opts.store ?? new MemorySessionStore());
    this.log = opts.logger ?? {};
    this.createPeer = opts.simplePeerFactory ?? ((o) => new SimplePeer(o));
    this.transport = new Transport(opts.transportFactory);
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
   * The underlying `simple-peer` instance, when one has been constructed.
   * Returns the {@link PeerLike} view; cast to `SimplePeer.Instance` if you
   * need the full stream API.
   */
  get simplePeer(): PeerLike | null {
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
    const message: ClientMessage = {
      type: 'join-room',
      roomId: input.roomId,
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

  /**
   * Gracefully end the session. Sends `close-room`, destroys the peer, and
   * closes the transport. Suppresses any reconnect attempt.
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
    this.destroyPeer();
    this.transport.close(CloseCodeEnum.ROOM_CLOSED, reason);
    this.setStatus('closed');
    this.emitClose(reason);
  }

  /** Hard cleanup without sending `close-room`. Use on fatal local errors. */
  destroy(): void {
    this.userClosed = true;
    this.clearReconnectTimer();
    this.destroyPeer();
    this.transport.close();
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
    // readyState 1 = OPEN in both browser WebSocket and `ws`.
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
   */
  private maybeBuildPeer(opts: { initiator: boolean }): boolean {
    if (this.peer) return false;
    const initiator = opts.initiator;
    const peer = this.createPeer({ ...this.simplePeerOpts, initiator });
    this.wirePeer(peer);
    this.peer = peer;
    this.seq.reset();
    this.log.info?.('constructed SimplePeer', { initiator });
    if (!this.socketReleased) this.setStatus('signaling');
    return true;
  }

  private wirePeer(peer: PeerLike): void {
    peer.on('signal', (data) => this.sendSignal(data));
    peer.on('connect', () => {
      this.log.info?.('peer connected');
      this.setStatus('connected');
      this.sendPeerConnected();
      this.emit('connect', undefined);
    });
    peer.on('data', (chunk) => this.handlePeerData(chunk));
    peer.on('stream', (stream) => this.emit('stream', stream));
    peer.on('track', (track, stream) => this.emit('track', { track, stream }));
    peer.on('close', () => {
      this.log.info?.('peer close');
      this.peer = null;
      if (!this.userClosed && !this.terminal) {
        // The P2P connection died. If the socket is still attached, the server
        // is still around — rejoin to re-signal. If the socket was released,
        // we need a fresh rejoin to recreate the room.
        this.triggerReconnect(new SignalingError(0, 'peer closed', { retryable: true }));
      }
    });
    peer.on('error', (err) => {
      this.log.error?.('peer error', { message: err.message });
      // simple-peer emits `close` after `error`, which drives reconnect.
    });
  }

  private destroyPeer(): void {
    if (!this.peer) return;
    const p = this.peer;
    this.peer = null;
    try {
      p.destroy();
    } catch {
      /* ignore */
    }
  }

  /** Destroy the current SimplePeer and build a fresh one with a new epoch. */
  private rebuildPeerWithNewEpoch(): void {
    this.destroyPeer();
    // Note: we do NOT change our own epoch here. A peer-reset from the server
    // means the *other* side changed epoch; our epoch stays the same and our
    // seq counter resets only because we built a new SimplePeer.
    this.maybeBuildPeer({ initiator: this.state?.role === 'host' });
  }

  // -------------------------------------------------------------------------
  // Signaling relay
  // -------------------------------------------------------------------------

  private sendSignal(data: SimplePeer.SignalData): void {
    const payload = JSON.stringify(data);
    if (this.socketReleased && this.peer?.connected) {
      // After the server released the sockets, carry renegotiation over the
      // data channel. See DESIGN.md "Renegotiation after socket release".
      const frame: DataChannelFrame = { kind: 'renegotiate', signal: data };
      try {
        this.peer.send(JSON.stringify(frame));
      } catch (e) {
        this.log.error?.('failed to send renegotiate frame', { message: (e as Error).message });
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
      this.peer.signal(data as SimplePeer.SignalData);
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
  // Data channel framing (renegotiation after socket release)
  // -------------------------------------------------------------------------

  private handlePeerData(chunk: unknown): void {
    // Emit raw data to the application.
    this.emit('data', chunk);
    // If it's a string, try to interpret as a control frame.
    if (typeof chunk !== 'string') return;
    let frame: unknown;
    try {
      frame = JSON.parse(chunk);
    } catch {
      return;
    }
    if (!isDataChannelFrame(frame)) return;
    if (frame.kind === 'renegotiate' && this.peer) {
      try {
        this.peer.signal(frame.signal);
      } catch (e) {
        this.log.warn?.('renegotiate peer.signal threw', { message: (e as Error).message });
      }
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
    this.destroyPeer();
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

function isDataChannelFrame(v: unknown): v is DataChannelFrame {
  if (!v || typeof v !== 'object') return false;
  const o = v as { kind?: unknown };
  return o.kind === 'renegotiate';
}
