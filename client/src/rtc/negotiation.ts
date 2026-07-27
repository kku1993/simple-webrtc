import type { Logger } from '../logger.js';
import { type SignalData, signalFrame } from './signal.js';

export interface NegotiatorOptions {
  pc: RTCPeerConnection;
  initiator: boolean;
  offerOptions?: RTCOfferOptions;
  answerOptions?: RTCAnswerOptions;
  sdpTransform?: (sdp: string) => string;
  log: Logger;
  /** Emit a frame for relay to the remote peer. */
  emitSignal(data: SignalData): void;
  /** Report an unrecoverable failure. The peer tears down. */
  onFatal(err: Error): void;
  /** True once the owning peer has been destroyed; all work short-circuits. */
  isDestroyed(): boolean;
  /** Called when signaling returns to `stable` with no queued renegotiation. */
  onStable(): void;
}

/**
 * Drives offer/answer.
 *
 * The model is asymmetric and initiator-driven: only the initiator ever calls
 * `createOffer`, and the responder asks for renegotiation by signaling
 * `{t:'renegotiate'}`. Because the protocol assigns host/guest at room creation
 * and never reassigns them, both peers agree permanently on who offers — so
 * offer/answer glare cannot occur and none of the "perfect negotiation"
 * rollback machinery is needed.
 */
export class Negotiator {
  private readonly o: NegotiatorOptions;

  /** Collapses several synchronous mutations into a single offer. */
  private batched = false;
  /** True between starting an offer/answer exchange and returning to `stable`. */
  private negotiating = false;
  /** A renegotiation was requested while one was already in flight. */
  private queued = false;
  /** The responder discards its own initial negotiation request. */
  private first = true;

  constructor(opts: NegotiatorOptions) {
    this.o = opts;
  }

  get isNegotiating(): boolean {
    return this.negotiating;
  }

  /**
   * Request negotiation, batching every request made in the same microtask.
   *
   * Two `addTrack` calls in a row must produce one offer, not two — without
   * this, each mutation would start its own exchange and the second would be
   * queued behind the first for no reason.
   */
  needsNegotiation(): void {
    if (this.batched) return;
    this.batched = true;
    queueMicrotask(() => {
      this.batched = false;
      if (this.o.isDestroyed()) return;
      // The responder's *first* request is discarded: the initiator is about to
      // offer anyway, and asking for a renegotiation before the initial exchange
      // has happened would race it.
      if (this.o.initiator || !this.first) {
        this.negotiate();
      } else {
        this.o.log.debug?.('discarding responder initial negotiation request');
      }
      this.first = false;
    });
  }

  /** Start (or queue) an offer/answer exchange. */
  negotiate(): void {
    if (this.o.isDestroyed()) return;
    if (this.negotiating) {
      // Never call createOffer while signalingState !== 'stable'.
      this.queued = true;
      this.o.log.debug?.('negotiation in flight; queueing');
      return;
    }
    this.negotiating = true;
    if (this.o.initiator) {
      void this.createOffer();
    } else {
      this.o.log.debug?.('requesting negotiation from initiator');
      this.o.emitSignal(signalFrame({ t: 'renegotiate' }));
    }
  }

  /**
   * Called when `signalingState` becomes `stable`. Runs exactly one queued
   * renegotiation, so a burst of requests during an exchange collapses into a
   * single follow-up.
   */
  handleStable(): void {
    this.negotiating = false;
    if (this.queued) {
      this.queued = false;
      this.o.log.debug?.('flushing queued negotiation');
      this.needsNegotiation();
      return;
    }
    this.o.onStable();
  }

  /** Produce an answer for a remote offer that has already been applied. */
  async createAnswer(): Promise<void> {
    if (this.o.isDestroyed()) return;
    try {
      const answer = await this.o.pc.createAnswer(this.o.answerOptions);
      if (this.o.isDestroyed()) return;
      if (answer.sdp && this.o.sdpTransform) answer.sdp = this.o.sdpTransform(answer.sdp);
      await this.o.pc.setLocalDescription(answer);
      if (this.o.isDestroyed()) return;
      const local = this.o.pc.localDescription ?? answer;
      this.o.emitSignal(signalFrame({ t: 'answer', sdp: local.sdp ?? '' }));
    } catch (e) {
      this.o.onFatal(wrap(e, 'Failed to create answer'));
    }
  }

  private async createOffer(): Promise<void> {
    if (this.o.isDestroyed()) return;
    try {
      const offer = await this.o.pc.createOffer(this.o.offerOptions);
      if (this.o.isDestroyed()) return;
      if (offer.sdp && this.o.sdpTransform) offer.sdp = this.o.sdpTransform(offer.sdp);
      await this.o.pc.setLocalDescription(offer);
      if (this.o.isDestroyed()) return;
      const local = this.o.pc.localDescription ?? offer;
      this.o.emitSignal(signalFrame({ t: 'offer', sdp: local.sdp ?? '' }));
    } catch (e) {
      this.o.onFatal(wrap(e, 'Failed to create offer'));
    }
  }
}

function wrap(e: unknown, message: string): Error {
  const err = new Error(`${message}: ${(e as Error)?.message ?? String(e)}`, { cause: e });
  err.name = 'RtcNegotiationError';
  return err;
}
