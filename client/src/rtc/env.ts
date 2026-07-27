/**
 * The single injection point for the WebRTC implementation.
 *
 * RTC is browser-only for supported purposes: the client's Node support covers
 * the signaling and state-machine layer, which the fakes-based test suite
 * exercises without any `RTCPeerConnection` at all. Consumers who want real
 * WebRTC under Node may supply `node-datachannel` or `wrtc` here, but that
 * configuration is neither tested nor supported.
 */
export interface RtcEnv {
  RTCPeerConnection: typeof RTCPeerConnection;
}

/**
 * Resolve the WebRTC implementation, preferring an explicit override and
 * falling back to the global `RTCPeerConnection`.
 *
 * @throws {Error} when no implementation is available. The message names the
 *   `rtcImpl` option explicitly, since "RTCPeerConnection is not defined" is a
 *   confusing thing to hit from a library.
 */
export function resolveRtcEnv(override?: Partial<RtcEnv>): RtcEnv {
  const impl =
    override?.RTCPeerConnection ??
    (globalThis as { RTCPeerConnection?: typeof RTCPeerConnection }).RTCPeerConnection;
  if (!impl) {
    throw new Error(
      'No RTCPeerConnection available. This environment has no native WebRTC; ' +
        'pass `rtcImpl: { RTCPeerConnection }` to supply one.',
    );
  }
  return { RTCPeerConnection: impl };
}

/** True when this environment can construct an `RTCPeerConnection`. */
export function hasWebRtcSupport(): boolean {
  return (
    typeof (globalThis as { RTCPeerConnection?: unknown }).RTCPeerConnection === 'function'
  );
}
