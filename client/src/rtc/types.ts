import type { Logger } from '../logger.js';
import type { RtcEnv } from './env.js';
import type { SignalData } from './signal.js';
import type { DataChannelHandle } from './channel-handle.js';

/**
 * What {@link DataChannelHandle.send} does when the channel is not open.
 *
 * The default is reliability-dependent, because neither policy is right for
 * both kinds of channel: flushing five stale cursor positions after a
 * reconnect is a bug, and silently dropping the first chat message of a session
 * is also a bug.
 *
 * - `'buffer'` — queue up to `bufferLimit` messages and flush them in order on
 *   open. Default for ordered, fully reliable channels.
 * - `'throw'` — throw {@link DataChannelNotOpenError}. Default for unordered or
 *   partially reliable channels.
 * - `'drop'` — discard silently.
 */
export type WhenClosedPolicy = 'buffer' | 'throw' | 'drop';

/** Declaration of one data channel. */
export interface DataChannelSpec {
  /**
   * Whether delivery order is guaranteed. Default `true`.
   *
   * Unordered channels deliver messages as they arrive, which is what you want
   * for state that supersedes itself (cursor positions, pose updates).
   */
  ordered?: boolean;
  /**
   * Maximum retransmission attempts before giving up on a message. `0` means
   * fire-and-forget. Mutually exclusive with {@link maxPacketLifeTime}.
   */
  maxRetransmits?: number;
  /**
   * Maximum milliseconds to spend retransmitting a message. Mutually exclusive
   * with {@link maxRetransmits}.
   */
  maxPacketLifeTime?: number;
  /** Subprotocol string, passed through to `RTCDataChannel`. */
  protocol?: string;
  /** Behavior of `send()` before the channel is open. See {@link WhenClosedPolicy}. */
  whenClosed?: WhenClosedPolicy;
  /** Maximum queued messages when `whenClosed` is `'buffer'`. Default 64. */
  bufferLimit?: number;
}

/** A {@link DataChannelSpec} with every default resolved. */
export interface ResolvedChannelSpec extends DataChannelSpec {
  ordered: boolean;
  whenClosed: WhenClosedPolicy;
  bufferLimit: number;
}

export interface RtcPeerOptions {
  /**
   * Whether this peer creates the offer. Derived from the protocol role: the
   * host is always the initiator, the guest never is, and the assignment never
   * changes for the life of a room. That fixed asymmetry is why this engine
   * needs no glare handling.
   */
  initiator: boolean;
  /** Passed to `new RTCPeerConnection(...)` — ICE servers, transport policy. */
  config?: RTCConfiguration;
  /** Passed to `createOffer()`. */
  offerOptions?: RTCOfferOptions;
  /** Passed to `createAnswer()`. */
  answerOptions?: RTCAnswerOptions;
  /** Hook to rewrite SDP before it is signaled. Applied to both offer and answer. */
  sdpTransform?: (sdp: string) => string;
  /**
   * Data channels to open, keyed by label. Both peers should declare the same
   * set; the initiator creates them and the responder binds by label, so the
   * ordering and reliability of a channel always come from the initiator and
   * the two sides cannot disagree.
   *
   * Labels beginning with `__sps_` are reserved for the engine.
   */
  dataChannels?: Record<string, DataChannelSpec>;
  /** Streams to attach at construction, equivalent to calling `addStream` for each. */
  streams?: MediaStream[];
  /**
   * A handle registry owned by the caller, reused across peer generations.
   *
   * `PeerConnection` passes the same map to every `RtcPeer` it builds so that an
   * application's `channel('chat')` reference keeps working after an epoch
   * change rebuilds the peer underneath it. When supplied, the peer never
   * disposes a handle — it only binds and unbinds the underlying channel.
   */
  channelHandles?: Map<string, DataChannelHandle>;
  /** Override the WebRTC implementation. See {@link RtcEnv}. */
  rtcImpl?: Partial<RtcEnv>;
  /** Structured logger; defaults to no-op. */
  logger?: Logger;
}

export interface RtcPeerEvents {
  /** A signal frame to relay to the remote peer. */
  signal: SignalData;
  /** Transport is connected and the control channel is open. */
  connect: void;
  /** A message arrived on the default data channel. */
  data: unknown;
  /** A remote stream was added. */
  stream: MediaStream;
  /** A remote track was added. */
  track: { track: MediaStreamTrack; stream: MediaStream };
  /** The peer closed, for any reason. Fires exactly once. */
  close: void;
  /** A fatal error. Always followed by `close`. */
  error: Error;
  /** A data channel reached the `open` state. */
  'channel-open': { label: string; channel: DataChannelHandle };
  /** A data channel closed, including via peer teardown. */
  'channel-close': { label: string };
  /** A message arrived on any non-control channel. */
  'channel-message': { label: string; data: unknown };
  /** A non-fatal data channel failure. */
  'channel-error': { label: string; message: string; cause?: unknown };
  /** The remote peer opened a channel we had not declared. */
  channel: { label: string; channel: DataChannelHandle };
  /** Renegotiation completed (signaling state returned to `stable`). */
  negotiated: void;
}

/** Snapshot of data channel state for diagnostics. */
export interface ChannelDiagnostics {
  label: string;
  readyState: RTCDataChannelState;
  ordered: boolean;
  bufferedAmount: number;
  queued: number;
  id: number | null;
}
