import type { Logger } from '../logger.js';

/**
 * Applies remote ICE candidates, buffering the ones that arrive too early.
 *
 * Candidates routinely arrive before the description they belong to — the
 * remote starts trickling as soon as it sets its local description, and the
 * relay makes no ordering guarantee between an SDP frame and the candidates
 * that follow it. `addIceCandidate` before `setRemoteDescription` throws, so
 * early candidates are queued and flushed once a remote description exists.
 */
export class IceManager {
  private pending: RTCIceCandidateInit[] = [];

  constructor(
    private readonly pc: RTCPeerConnection,
    private readonly log: Logger,
  ) {}

  /** Number of candidates waiting for a remote description. */
  get pendingCount(): number {
    return this.pending.length;
  }

  /** Apply a remote candidate now, or queue it until a remote description exists. */
  addRemote(candidate: RTCIceCandidateInit): void {
    if (this.pc.remoteDescription?.type) {
      void this.apply(candidate);
    } else {
      this.pending.push(candidate);
    }
  }

  /** Apply every queued candidate. Call after `setRemoteDescription` resolves. */
  flush(): void {
    if (this.pending.length === 0) return;
    const queued = this.pending;
    this.pending = [];
    this.log.debug?.('flushing pending ice candidates', { count: queued.length });
    for (const c of queued) void this.apply(c);
  }

  /** Drop queued candidates without applying them. */
  clear(): void {
    this.pending = [];
  }

  /**
   * A candidate that will not apply is never fatal.
   *
   * Browsers reject candidates for all sorts of benign reasons — unresolved
   * mDNS `.local` hostnames, a transport that has already closed, a stale
   * generation after renegotiation. ICE fails on its own if no candidate pair
   * works, and tearing the connection down over one rejected candidate turns a
   * recoverable situation into a dropped call.
   */
  private async apply(candidate: RTCIceCandidateInit): Promise<void> {
    try {
      await this.pc.addIceCandidate(candidate);
    } catch (e) {
      this.log.warn?.('ignoring unusable ICE candidate', {
        message: (e as Error)?.message,
      });
    }
  }
}
