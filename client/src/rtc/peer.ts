import { Emitter } from '../emitter.js';
import { type Logger, NOOP_LOGGER } from '../logger.js';
import {
  ChannelManager,
  CONTROL_CHANNEL_LABEL,
  DEFAULT_CHANNEL_LABEL,
  assertUsableLabel,
} from './channels.js';
import { type ChannelMessage, type DataChannelHandle } from './channel-handle.js';
import { resolveRtcEnv } from './env.js';
import { IceManager } from './ice.js';
import { MediaManager } from './media.js';
import { Negotiator } from './negotiation.js';
import { type SignalData, candidateToInit, parseSignalData, signalFrame } from './signal.js';
import type {
  ChannelDiagnostics,
  DataChannelSpec,
  RtcPeerEvents,
  RtcPeerOptions,
} from './types.js';

/**
 * A single WebRTC peer connection with media and any number of data channels.
 *
 * Replaces `simple-peer`. The scope is deliberately narrower: this engine
 * supports one fixed initiator, trickle ICE, unified plan, and modern browsers.
 * It does not attempt to be a general-purpose WebRTC library, and it does not
 * carry a Node stream interface.
 *
 * Lifecycle is owned by the caller. {@link PeerConnection} destroys and rebuilds
 * an `RtcPeer` whenever the remote's epoch changes, so this class has no ICE
 * restart, no connection resurrection, and no half-open recovery — a broken peer
 * is thrown away rather than repaired.
 */
export class RtcPeer extends Emitter<RtcPeerEvents> {
  /** Whether this peer creates offers. Fixed for the life of the instance. */
  readonly initiator: boolean;

  private readonly pc: RTCPeerConnection;
  private readonly log: Logger;
  private readonly ice: IceManager;
  private readonly media: MediaManager;
  private readonly negotiator: Negotiator;
  private readonly channels: ChannelManager;

  private destroyedFlag = false;
  private connectedFlag = false;
  /** True once the DTLS/ICE transport reports a usable connection. */
  private transportReady = false;

  constructor(opts: RtcPeerOptions) {
    super();
    this.initiator = opts.initiator;
    this.log = opts.logger ?? NOOP_LOGGER;

    const env = resolveRtcEnv(opts.rtcImpl);
    this.pc = new env.RTCPeerConnection(opts.config);

    this.ice = new IceManager(this.pc, this.log);

    this.media = new MediaManager(this.pc, this.log, {
      onTrack: (track, stream) => this.emit('track', { track, stream }),
      onStream: (stream) => this.emit('stream', stream),
      needsNegotiation: () => this.negotiator.needsNegotiation(),
    });

    this.negotiator = new Negotiator({
      pc: this.pc,
      initiator: opts.initiator,
      ...(opts.offerOptions !== undefined ? { offerOptions: opts.offerOptions } : {}),
      ...(opts.answerOptions !== undefined ? { answerOptions: opts.answerOptions } : {}),
      ...(opts.sdpTransform !== undefined ? { sdpTransform: opts.sdpTransform } : {}),
      log: this.log,
      emitSignal: (data) => this.emit('signal', data),
      onFatal: (err) => this.destroy(err),
      isDestroyed: () => this.destroyedFlag,
      onStable: () => this.emit('negotiated', undefined),
    });

    this.wirePeerConnection();

    for (const label of Object.keys(opts.dataChannels ?? {})) assertUsableLabel(label);
    this.channels = new ChannelManager({
      pc: this.pc,
      initiator: opts.initiator,
      declared: {
        [CONTROL_CHANNEL_LABEL]: {},
        [DEFAULT_CHANNEL_LABEL]: {},
        ...(opts.dataChannels ?? {}),
      },
      log: this.log,
      ...(opts.channelHandles !== undefined ? { handles: opts.channelHandles } : {}),
      hooks: {
        onOpen: (label, handle) => this.handleChannelOpen(label, handle),
        onClose: (label) => this.handleChannelClose(label),
        onMessage: (label, data) => this.handleChannelMessage(label, data),
        onError: (label, message, cause) => this.emitChannelError(label, message, cause),
        onRemoteChannel: (label, handle) => this.emit('channel', { label, channel: handle }),
      },
    });

    for (const stream of opts.streams ?? []) {
      try {
        this.media.addStream(stream);
      } catch (e) {
        this.log.warn?.('failed to add initial stream', { message: (e as Error)?.message });
      }
    }

    this.negotiator.needsNegotiation();
  }

  // --- state ---------------------------------------------------------------

  /** True once the transport is up and the control channel is open. */
  get connected(): boolean {
    return this.connectedFlag;
  }

  /** True once {@link destroy} has run. */
  get destroyed(): boolean {
    return this.destroyedFlag;
  }

  /**
   * The underlying `RTCPeerConnection`, for advanced use (transceivers, sender
   * parameters, encoding controls). The instance dies with the peer, so callers
   * must not cache it across rebuilds.
   */
  get peerConnection(): RTCPeerConnection {
    return this.pc;
  }

  // --- signaling -----------------------------------------------------------

  /**
   * Apply an inbound signal frame.
   *
   * Accepts `unknown` and validates: a frame this build does not recognize is
   * logged and dropped rather than treated as fatal, so a peer running a newer
   * client cannot take this one down by sending something new.
   */
  signal(raw: unknown): void {
    if (this.destroyedFlag) return;
    const data = parseSignalData(raw);
    if (!data) {
      this.log.warn?.('ignoring unrecognized signal frame');
      return;
    }
    switch (data.t) {
      case 'offer':
      case 'answer':
        void this.applyRemoteDescription(data.t, data.sdp);
        break;
      case 'candidate':
        this.ice.addRemote(data.candidate);
        break;
      case 'renegotiate':
        // Only the initiator offers, so only it can honor this.
        if (this.initiator) this.negotiator.needsNegotiation();
        break;
      case 'transceiver':
        if (this.initiator) this.addTransceiver(data.kind, data.init);
        break;
    }
  }

  /**
   * Send a signal frame over the control channel instead of the signaling
   * socket. Used after the server releases its sockets, when renegotiation has
   * to travel peer-to-peer.
   *
   * @returns whether the frame was handed to an open channel.
   */
  sendControlSignal(data: SignalData): boolean {
    const control = this.channels.control;
    if (!control.isOpen) return false;
    try {
      control.send(JSON.stringify(data));
      return true;
    } catch (e) {
      this.log.error?.('failed to send control frame', { message: (e as Error)?.message });
      return false;
    }
  }

  /**
   * Add a transceiver. On the responder this cannot be done locally without
   * creating an offer, so it is delegated to the initiator by signal.
   */
  addTransceiver(kind: string, init?: RTCRtpTransceiverInit): void {
    if (this.destroyedFlag) return;
    if (!this.initiator) {
      this.emit('signal', signalFrame(init ? { t: 'transceiver', kind, init } : { t: 'transceiver', kind }));
      return;
    }
    try {
      this.pc.addTransceiver(kind, init);
      this.negotiator.needsNegotiation();
    } catch (e) {
      this.destroy(new Error(`addTransceiver failed: ${(e as Error)?.message}`, { cause: e }));
    }
  }

  // --- data ----------------------------------------------------------------

  /** Send on the default application channel. */
  send(data: ChannelMessage): void {
    this.channels.defaultChannel.send(data);
  }

  /**
   * The handle for a declared or dynamic channel.
   *
   * @throws {Error} for a label that was never declared and is not open. Use
   *   {@link openChannel} to create one at runtime.
   */
  channel(label: string): DataChannelHandle {
    assertUsableLabel(label);
    const handle = this.channels.get(label);
    if (!handle) {
      throw new Error(
        `Unknown data channel "${label}". Declare it in options.dataChannels or call openChannel().`,
      );
    }
    return handle;
  }

  /** Open a channel that was not declared at construction. */
  openChannel(label: string, spec: DataChannelSpec = {}): DataChannelHandle {
    assertUsableLabel(label);
    return this.channels.open(label, spec);
  }

  /** Every channel handle, keyed by label, excluding engine-reserved channels. */
  get dataChannels(): ReadonlyMap<string, DataChannelHandle> {
    const out = new Map<string, DataChannelHandle>();
    for (const [label, handle] of this.channels.all) {
      if (!isReserved(label)) out.set(label, handle);
    }
    return out;
  }

  /** Per-channel diagnostics, including the reserved channels. */
  get channelDiagnostics(): ChannelDiagnostics[] {
    return [...this.channels.all.values()].map((h) => h.diagnostics);
  }

  // --- media ---------------------------------------------------------------

  addTrack(track: MediaStreamTrack, stream: MediaStream): void {
    if (this.destroyedFlag) return;
    this.media.addTrack(track, stream);
  }

  removeTrack(track: MediaStreamTrack, stream: MediaStream): void {
    if (this.destroyedFlag) return;
    this.media.removeTrack(track, stream);
  }

  replaceTrack(
    oldTrack: MediaStreamTrack,
    newTrack: MediaStreamTrack,
    stream: MediaStream,
  ): Promise<void> {
    if (this.destroyedFlag) return Promise.resolve();
    return this.media.replaceTrack(oldTrack, newTrack, stream);
  }

  addStream(stream: MediaStream): void {
    if (this.destroyedFlag) return;
    this.media.addStream(stream);
  }

  removeStream(stream: MediaStream): void {
    if (this.destroyedFlag) return;
    this.media.removeStream(stream);
  }

  /** The native stats report. Unlike `simple-peer`, nothing is flattened. */
  getStats(): Promise<RTCStatsReport> {
    return this.pc.getStats();
  }

  // --- teardown ------------------------------------------------------------

  /**
   * Tear down the peer. Idempotent. Emits `error` (when given one) followed by
   * `close`, exactly once.
   */
  destroy(err?: Error): void {
    if (this.destroyedFlag) return;
    this.destroyedFlag = true;
    this.connectedFlag = false;

    this.channels.dispose();
    this.media.clear();
    this.ice.clear();

    this.pc.onicecandidate = null;
    this.pc.oniceconnectionstatechange = null;
    this.pc.onconnectionstatechange = null;
    this.pc.onsignalingstatechange = null;
    this.pc.ondatachannel = null;
    this.pc.ontrack = null;
    try {
      this.pc.close();
    } catch {
      /* already closed */
    }

    if (err) {
      this.log.error?.('peer destroyed with error', { message: err.message });
      this.emit('error', err);
    }
    this.emit('close', undefined);
  }

  // --- private -------------------------------------------------------------

  private wirePeerConnection(): void {
    this.pc.onicecandidate = (event: RTCPeerConnectionIceEvent): void => {
      if (this.destroyedFlag || !event.candidate) return;
      this.emit('signal', signalFrame({ t: 'candidate', candidate: candidateToInit(event.candidate) }));
    };

    this.pc.onconnectionstatechange = (): void => {
      if (this.destroyedFlag) return;
      const state = this.pc.connectionState;
      this.log.debug?.('connectionState', { state });
      if (state === 'connected') {
        this.transportReady = true;
        this.maybeConnect();
      } else if (state === 'failed') {
        this.destroy(new Error('WebRTC connection failed'));
      } else if (state === 'closed') {
        this.destroy();
      }
    };

    this.pc.oniceconnectionstatechange = (): void => {
      if (this.destroyedFlag) return;
      const state = this.pc.iceConnectionState;
      this.log.debug?.('iceConnectionState', { state });
      // `connectionState` is the authority, but ICE reaching connected is a
      // sufficient signal on implementations that report it first.
      if (state === 'connected' || state === 'completed') {
        this.transportReady = true;
        this.maybeConnect();
      } else if (state === 'failed') {
        this.destroy(new Error('ICE connection failed'));
      }
    };

    this.pc.onsignalingstatechange = (): void => {
      if (this.destroyedFlag) return;
      if (this.pc.signalingState !== 'stable') return;
      const flushed = this.media.flushPendingRemovals();
      this.negotiator.handleStable();
      if (flushed) this.negotiator.needsNegotiation();
    };

    this.pc.ontrack = (event: RTCTrackEvent): void => {
      if (this.destroyedFlag) return;
      this.media.handleTrackEvent(event);
    };
  }

  private async applyRemoteDescription(type: 'offer' | 'answer', sdp: string): Promise<void> {
    try {
      await this.pc.setRemoteDescription({ type, sdp });
      if (this.destroyedFlag) return;
      // Candidates that arrived ahead of this description can now be applied.
      this.ice.flush();
      if (type === 'offer') await this.negotiator.createAnswer();
    } catch (e) {
      this.destroy(
        new Error(`Failed to apply remote ${type}: ${(e as Error)?.message}`, { cause: e }),
      );
    }
  }

  private handleChannelOpen(label: string, handle: DataChannelHandle): void {
    if (label === CONTROL_CHANNEL_LABEL) {
      this.maybeConnect();
      return;
    }
    if (isReserved(label)) return;
    this.emit('channel-open', { label, channel: handle });
  }

  private handleChannelClose(label: string): void {
    if (isReserved(label)) return;
    this.emit('channel-close', { label });
  }

  private handleChannelMessage(label: string, data: unknown): void {
    if (label === CONTROL_CHANNEL_LABEL) {
      this.handleControlFrame(data);
      return;
    }
    if (label === DEFAULT_CHANNEL_LABEL) {
      this.emit('data', data);
      return;
    }
    this.emit('channel-message', { label, data });
  }

  /**
   * Control frames are renegotiation signals routed peer-to-peer. Because they
   * have their own channel, there is no need to sniff application traffic for a
   * discriminator — and no way for application data to be mistaken for one.
   */
  private handleControlFrame(data: unknown): void {
    if (typeof data !== 'string') return;
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      this.log.warn?.('unparseable control frame');
      return;
    }
    this.signal(parsed);
  }

  private emitChannelError(label: string, message: string, cause?: unknown): void {
    this.log.warn?.('channel error', { label, message });
    this.emit('channel-error', cause === undefined ? { label, message } : { label, message, cause });
  }

  private maybeConnect(): void {
    if (this.connectedFlag || this.destroyedFlag) return;
    if (!this.transportReady || !this.channels.control.isOpen) return;
    this.connectedFlag = true;
    const unbound = this.channels.unboundDeclaredLabels.filter((l) => !isReserved(l));
    if (unbound.length > 0) {
      // Only reachable on the responder, and only when the two sides declared
      // different channel sets.
      this.log.warn?.('declared channels never opened; peer did not create them', {
        labels: unbound.join(','),
      });
    }
    this.log.info?.('peer connected');
    this.emit('connect', undefined);
  }
}

function isReserved(label: string): boolean {
  return label === CONTROL_CHANNEL_LABEL || label === DEFAULT_CHANNEL_LABEL;
}
