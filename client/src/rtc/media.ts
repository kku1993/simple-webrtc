import type { Logger } from '../logger.js';

export interface MediaManagerHooks {
  /** A remote track arrived, once per (track, stream) pair. */
  onTrack(track: MediaStreamTrack, stream: MediaStream): void;
  /** A remote stream arrived, once per stream id. */
  onStream(stream: MediaStream): void;
  /** A local mutation requires a fresh offer. */
  needsNegotiation(): void;
}

/**
 * Owns the outbound sender map and inbound track/stream bookkeeping.
 *
 * Senders are keyed by the `(track, stream)` pair rather than by track alone,
 * because the same track can legitimately be sent as part of two different
 * streams and each pairing gets its own `RTCRtpSender`.
 */
export class MediaManager {
  private readonly senders = new Map<MediaStreamTrack, Map<MediaStream, RTCRtpSender>>();
  /**
   * Senders that have been removed. Re-adding one is refused rather than
   * silently producing a sender that never transmits.
   */
  private readonly removed = new WeakSet<RTCRtpSender>();
  /**
   * Senders whose removal was rejected because signaling was mid-exchange.
   * Retried when state returns to `stable`.
   */
  private awaitingStable: RTCRtpSender[] = [];
  private readonly remoteStreamIds = new Set<string>();

  constructor(
    private readonly pc: RTCPeerConnection,
    private readonly log: Logger,
    private readonly hooks: MediaManagerHooks,
  ) {}

  /** Number of live outbound senders. */
  get senderCount(): number {
    let n = 0;
    for (const submap of this.senders.values()) n += submap.size;
    return n;
  }

  addTrack(track: MediaStreamTrack, stream: MediaStream): void {
    const submap = this.senders.get(track) ?? new Map<MediaStream, RTCRtpSender>();
    const existing = submap.get(stream);
    if (existing) {
      if (this.removed.has(existing)) {
        throw new Error(
          'Track has been removed from this peer. Toggle `track.enabled` instead of ' +
            're-adding a removed track.',
        );
      }
      throw new Error('Track has already been added to that stream.');
    }
    const sender = this.pc.addTrack(track, stream);
    submap.set(stream, sender);
    this.senders.set(track, submap);
    this.hooks.needsNegotiation();
  }

  removeTrack(track: MediaStreamTrack, stream: MediaStream): void {
    const sender = this.senders.get(track)?.get(stream);
    if (!sender) throw new Error('Cannot remove a track that was never added.');
    this.removed.add(sender);
    try {
      this.pc.removeTrack(sender);
    } catch (e) {
      // Removing a sender can be rejected while an exchange is in flight.
      // Defer rather than fail: the retry on `stable` is always safe.
      this.log.debug?.('deferring removeTrack until signaling is stable', {
        message: (e as Error)?.message,
      });
      this.awaitingStable.push(sender);
    }
    this.hooks.needsNegotiation();
  }

  replaceTrack(
    oldTrack: MediaStreamTrack,
    newTrack: MediaStreamTrack,
    stream: MediaStream,
  ): Promise<void> {
    const submap = this.senders.get(oldTrack);
    const sender = submap?.get(stream);
    if (!submap || !sender) {
      return Promise.reject(new Error('Cannot replace a track that was never added.'));
    }
    // Re-key the sender from (oldTrack, stream) to (newTrack, stream) so a
    // later remove or replace still finds it.
    if (oldTrack !== newTrack) {
      submap.delete(stream);
      if (submap.size === 0) this.senders.delete(oldTrack);
      const newSubmap = this.senders.get(newTrack) ?? new Map<MediaStream, RTCRtpSender>();
      newSubmap.set(stream, sender);
      this.senders.set(newTrack, newSubmap);
    }
    // replaceTrack does not renegotiate — that is the point of it.
    return sender.replaceTrack(newTrack);
  }

  addStream(stream: MediaStream): void {
    for (const track of stream.getTracks()) this.addTrack(track, stream);
  }

  removeStream(stream: MediaStream): void {
    for (const track of stream.getTracks()) this.removeTrack(track, stream);
  }

  /** Retry removals that were rejected mid-exchange. Call on `stable`. */
  flushPendingRemovals(): boolean {
    if (this.awaitingStable.length === 0) return false;
    const queued = this.awaitingStable;
    this.awaitingStable = [];
    for (const sender of queued) {
      try {
        this.pc.removeTrack(sender);
      } catch (e) {
        this.log.warn?.('removeTrack failed after stable', {
          message: (e as Error)?.message,
        });
      }
    }
    return true;
  }

  /**
   * Handle an inbound `track` event.
   *
   * Two subtleties, both easy to get wrong and both load-bearing:
   *
   * 1. A stream fires `track` once per track but must fire `stream` only once,
   *    so stream ids are deduplicated.
   * 2. `stream` is emitted on a microtask, so that by the time a consumer
   *    receives it every track belonging to that stream has been attached.
   *    Emitting synchronously hands out a stream that is still filling up.
   */
  handleTrackEvent(event: RTCTrackEvent): void {
    for (const stream of event.streams) {
      this.hooks.onTrack(event.track, stream);
      if (this.remoteStreamIds.has(stream.id)) continue;
      this.remoteStreamIds.add(stream.id);
      queueMicrotask(() => this.hooks.onStream(stream));
    }
  }

  /** Drop all bookkeeping. The peer is going away. */
  clear(): void {
    this.senders.clear();
    this.awaitingStable = [];
    this.remoteStreamIds.clear();
  }
}
