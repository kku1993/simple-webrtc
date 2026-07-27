import { Emitter } from '../emitter.js';
import { DataChannelNotOpenError } from '../errors.js';
import type { ResolvedChannelSpec, ChannelDiagnostics } from './types.js';

/** Anything an `RTCDataChannel` will accept. */
export type ChannelMessage = string | Blob | ArrayBuffer | ArrayBufferView;

/** Default `bufferedAmountLowThreshold`, and the point at which `drain` fires. */
export const DEFAULT_LOW_THRESHOLD = 64 * 1024;

export interface DataChannelEvents {
  /** The underlying channel reached `open`. Any queued messages have flushed. */
  open: void;
  /** The underlying channel closed, or the peer generation was torn down. */
  close: void;
  /** A message arrived. `string` for text, `ArrayBuffer` for binary. */
  message: unknown;
  /** A non-fatal failure (send error, queue overflow). */
  error: { message: string; cause?: unknown };
  /** `bufferedAmount` fell back below the low threshold. */
  drain: void;
}

/**
 * A stable, application-facing handle for one logical data channel.
 *
 * Handle identity survives peer rebuilds: an application holds the object
 * returned by `peer.channel('chat')` for the lifetime of the connection and
 * never re-fetches it after an epoch change. The underlying `RTCDataChannel` is
 * replaced on every peer generation, which the handle absorbs by re-binding.
 *
 * The contract that matters: **handle identity is stable, channel state is
 * not.** Gate sends on {@link readyState}, or let {@link ResolvedChannelSpec.whenClosed}
 * do it for you.
 */
export class DataChannelHandle extends Emitter<DataChannelEvents> {
  readonly label: string;
  readonly spec: ResolvedChannelSpec;

  private channel: RTCDataChannel | null = null;
  private queue: ChannelMessage[] = [];
  /** True while `bufferedAmount` is above the low threshold, so `drain` fires once. */
  private draining = false;
  private closed = false;

  constructor(label: string, spec: ResolvedChannelSpec) {
    super();
    this.label = label;
    this.spec = spec;
  }

  /** Whether delivery order is guaranteed on this channel. */
  get ordered(): boolean {
    return this.spec.ordered;
  }

  /**
   * The underlying channel's state, or `'connecting'` when no channel is bound
   * — which is what a handle reports before the peer exists and between peer
   * generations.
   */
  get readyState(): RTCDataChannelState {
    if (this.closed) return 'closed';
    return this.channel?.readyState ?? 'connecting';
  }

  /** Bytes queued in the underlying channel's send buffer. */
  get bufferedAmount(): number {
    return this.channel?.bufferedAmount ?? 0;
  }

  /** Messages held in the handle's own queue awaiting `open`. */
  get queuedCount(): number {
    return this.queue.length;
  }

  /** The SCTP stream id, once the channel is bound and the transport is up. */
  get id(): number | null {
    return this.channel?.id ?? null;
  }

  /** True when the channel is bound and open. */
  get isOpen(): boolean {
    return this.channel?.readyState === 'open';
  }

  /**
   * Send a message.
   *
   * When the channel is not open, behavior follows the spec's `whenClosed`
   * policy: queue it, throw, or drop.
   *
   * @throws {DataChannelNotOpenError} when the channel is not open and the
   *   policy is `'throw'`.
   */
  send(data: ChannelMessage): void {
    const ch = this.channel;
    if (ch && ch.readyState === 'open') {
      try {
        this.rawSend(ch, data);
        this.trackBackpressure(ch);
      } catch (e) {
        this.emitError('send failed', e);
      }
      return;
    }
    switch (this.spec.whenClosed) {
      case 'buffer':
        if (this.queue.length >= this.spec.bufferLimit) {
          // Drop the oldest so a stalled channel cannot grow without bound.
          this.queue.shift();
          this.emitError(
            `send queue full (limit ${this.spec.bufferLimit}); dropped oldest message`,
          );
        }
        this.queue.push(data);
        return;
      case 'drop':
        return;
      case 'throw':
        throw new DataChannelNotOpenError(this.label, this.readyState);
    }
  }

  /** Close the underlying channel. The handle stays usable across rebuilds. */
  close(): void {
    const ch = this.channel;
    if (!ch) return;
    try {
      ch.close();
    } catch {
      /* already gone */
    }
  }

  /** Diagnostics snapshot. Carries no message contents. */
  get diagnostics(): ChannelDiagnostics {
    return {
      label: this.label,
      readyState: this.readyState,
      ordered: this.spec.ordered,
      bufferedAmount: this.bufferedAmount,
      queued: this.queue.length,
      id: this.id,
    };
  }

  // --- internal: driven by ChannelManager ----------------------------------

  /**
   * Attach an `RTCDataChannel` to this handle.
   *
   * @internal
   */
  bind(channel: RTCDataChannel): void {
    if (this.channel === channel) return;
    this.unbind();
    this.channel = channel;
    channel.binaryType = 'arraybuffer';
    if (typeof channel.bufferedAmountLowThreshold === 'number') {
      channel.bufferedAmountLowThreshold = DEFAULT_LOW_THRESHOLD;
    }
    channel.onopen = (): void => this.handleOpen();
    channel.onclose = (): void => this.handleClose();
    channel.onerror = (ev): void => {
      const err = (ev).error as Error | undefined;
      this.emitError(err?.message ?? 'data channel error', err);
    };
    channel.onmessage = (ev: MessageEvent): void => this.emit('message', ev.data);
    channel.onbufferedamountlow = (): void => {
      if (!this.draining) return;
      this.draining = false;
      this.emit('drain', undefined);
    };
    // A channel handed to us by `ondatachannel` may already be open, in which
    // case `onopen` will never fire.
    if (channel.readyState === 'open') this.handleOpen();
  }

  /**
   * Detach the current channel, firing `close` if it was open. Called on peer
   * teardown; queued messages are retained for the next generation.
   *
   * @internal
   */
  unbind(): void {
    const ch = this.channel;
    if (!ch) return;
    const wasOpen = ch.readyState === 'open';
    ch.onopen = null;
    ch.onclose = null;
    ch.onerror = null;
    ch.onmessage = null;
    ch.onbufferedamountlow = null;
    this.channel = null;
    this.draining = false;
    if (wasOpen) this.emit('close', undefined);
  }

  /**
   * Permanently retire the handle: unbind, drop queued messages, and refuse
   * further sends. Called when the owning connection closes for good.
   *
   * @internal
   */
  dispose(): void {
    this.unbind();
    this.queue = [];
    this.closed = true;
    this.removeAllListeners();
  }

  // --- private -------------------------------------------------------------

  private handleOpen(): void {
    const ch = this.channel;
    if (!ch) return;
    this.flushQueue(ch);
    this.emit('open', undefined);
  }

  private handleClose(): void {
    this.channel = null;
    this.draining = false;
    this.emit('close', undefined);
  }

  private flushQueue(ch: RTCDataChannel): void {
    if (this.queue.length === 0) return;
    const pending = this.queue;
    this.queue = [];
    for (const msg of pending) {
      try {
        this.rawSend(ch, msg);
      } catch (e) {
        this.emitError('queued send failed', e);
        break;
      }
    }
    this.trackBackpressure(ch);
  }

  private trackBackpressure(ch: RTCDataChannel): void {
    if (!this.draining && ch.bufferedAmount > DEFAULT_LOW_THRESHOLD) {
      this.draining = true;
    }
  }

  private rawSend(ch: RTCDataChannel, data: ChannelMessage): void {
    // RTCDataChannel.send is declared as four overloads; the union does not
    // match any single one, so dispatch on the runtime type.
    if (typeof data === 'string') ch.send(data);
    else if (data instanceof ArrayBuffer) ch.send(data);
    // `ArrayBuffer.isView` narrows to ArrayBufferLike, which admits
    // SharedArrayBuffer; `send` accepts only ArrayBuffer-backed views.
    else if (ArrayBuffer.isView(data)) ch.send(data as ArrayBufferView<ArrayBuffer>);
    else ch.send(data);
  }

  private emitError(message: string, cause?: unknown): void {
    this.emit('error', cause === undefined ? { message } : { message, cause });
  }
}
