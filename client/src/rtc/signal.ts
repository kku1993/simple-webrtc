/**
 * The peer-to-peer signal wire format.
 *
 * `PeerConnection` JSON-stringifies these into the `data` field of the
 * protocol's `signal` message and the server relays them opaquely (see
 * `docs/DESIGN.md`), so this is a wire format between two independently
 * deployed clients — not an internal detail.
 *
 * Two deliberate departures from how `simple-peer` framed the same data:
 *
 * 1. **Explicit discriminant.** `simple-peer` dispatched on field *presence*
 *    (`data.sdp` / `data.candidate` / `data.renegotiate`), so a candidate frame
 *    carried no type at all. Every frame here carries `t`.
 * 2. **Unknown frames are ignored, not fatal.** `simple-peer` destroyed the
 *    peer when a frame matched none of its known shapes, which makes protocol
 *    evolution impossible without a flag day. {@link parseSignalData} returns
 *    `null` and the caller logs and continues.
 */

/** Version of the signal envelope. Bumped only on a breaking shape change. */
export const SIGNAL_VERSION = 1;

/** The payload variants, discriminated by `t`. */
export type SignalPayload =
  /** SDP offer from the initiator. */
  | { t: 'offer'; sdp: string }
  /** SDP answer from the responder. */
  | { t: 'answer'; sdp: string }
  /** A trickled ICE candidate. */
  | { t: 'candidate'; candidate: RTCIceCandidateInit }
  /** Responder asking the initiator to produce a fresh offer. */
  | { t: 'renegotiate' }
  /** Responder asking the initiator to add a transceiver on its behalf. */
  | { t: 'transceiver'; kind: string; init?: RTCRtpTransceiverInit };

/** A complete signal frame: a payload plus the envelope version. */
export type SignalData = SignalPayload & { v: number };

/** Stamp the current envelope version onto a payload. */
export function signalFrame<T extends SignalPayload>(payload: T): T & { v: number } {
  return { ...payload, v: SIGNAL_VERSION };
}

/**
 * Parse and validate an incoming signal frame.
 *
 * Returns `null` for anything unrecognized — an unknown `t`, a future envelope
 * version, a malformed payload. Callers log and continue; a bad frame from the
 * peer must never tear down the connection.
 */
export function parseSignalData(raw: unknown): SignalData | null {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null;
  const o = raw as Record<string, unknown>;

  // Reject envelope versions we do not understand. Same-version frames with an
  // unknown `t` also fall through to `null` below, so additive payload variants
  // are safe to introduce without bumping `v`.
  if (o.v !== undefined && o.v !== SIGNAL_VERSION) return null;
  const v = SIGNAL_VERSION;

  switch (o.t) {
    case 'offer':
      return typeof o.sdp === 'string' ? { v, t: 'offer', sdp: o.sdp } : null;
    case 'answer':
      return typeof o.sdp === 'string' ? { v, t: 'answer', sdp: o.sdp } : null;
    case 'candidate': {
      const c = o.candidate;
      if (typeof c !== 'object' || c === null) return null;
      const cand = c as Record<string, unknown>;
      // `candidate` may legitimately be the empty string (end-of-candidates).
      if (typeof cand.candidate !== 'string') return null;
      const init: RTCIceCandidateInit = { candidate: cand.candidate };
      if (typeof cand.sdpMid === 'string') init.sdpMid = cand.sdpMid;
      if (typeof cand.sdpMLineIndex === 'number') init.sdpMLineIndex = cand.sdpMLineIndex;
      if (typeof cand.usernameFragment === 'string') init.usernameFragment = cand.usernameFragment;
      return { v, t: 'candidate', candidate: init };
    }
    case 'renegotiate':
      return { v, t: 'renegotiate' };
    case 'transceiver': {
      if (typeof o.kind !== 'string') return null;
      const frame: SignalData = { v, t: 'transceiver', kind: o.kind };
      if (typeof o.init === 'object' && o.init !== null) {
        frame.init = o.init;
      }
      return frame;
    }
    default:
      return null;
  }
}

/**
 * Serialize an `RTCIceCandidate` for the wire.
 *
 * Only the fields needed to reconstruct the candidate are copied; the rest of
 * the object (`address`, `port`, `foundation`, …) is derived from the candidate
 * string on the far side and would leak IPs into logs for no benefit.
 */
export function candidateToInit(candidate: RTCIceCandidate): RTCIceCandidateInit {
  const init: RTCIceCandidateInit = { candidate: candidate.candidate };
  if (candidate.sdpMid !== null) init.sdpMid = candidate.sdpMid;
  if (candidate.sdpMLineIndex !== null) init.sdpMLineIndex = candidate.sdpMLineIndex;
  if (candidate.usernameFragment) init.usernameFragment = candidate.usernameFragment;
  return init;
}
