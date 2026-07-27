// The in-house WebRTC engine. Replaces `simple-peer`.
//
// `RtcPeer` is the only entry point most code needs; the manager classes are
// exported for tests and for consumers doing something unusual.

export { RtcPeer } from './peer.js';
export {
  ChannelManager,
  CONTROL_CHANNEL_LABEL,
  DEFAULT_CHANNEL_LABEL,
  RESERVED_PREFIX,
  assertUsableLabel,
  resolveSpec,
} from './channels.js';
export {
  DataChannelHandle,
  DEFAULT_LOW_THRESHOLD,
  type ChannelMessage,
  type DataChannelEvents,
} from './channel-handle.js';
export { IceManager } from './ice.js';
export { MediaManager, type MediaManagerHooks } from './media.js';
export { Negotiator, type NegotiatorOptions } from './negotiation.js';
export {
  SIGNAL_VERSION,
  candidateToInit,
  parseSignalData,
  signalFrame,
  type SignalData,
  type SignalPayload,
} from './signal.js';
export { hasWebRtcSupport, resolveRtcEnv, type RtcEnv } from './env.js';
export type {
  ChannelDiagnostics,
  DataChannelSpec,
  ResolvedChannelSpec,
  RtcPeerEvents,
  RtcPeerOptions,
  WhenClosedPolicy,
} from './types.js';
