// Public entry point for the peer-client package.
//
// A TypeScript client that wraps `simple-peer` and speaks the signaling
// protocol described in `docs/DESIGN.md`. See the README for usage.

export {
  PeerConnection,
  SignalingError,
  DataChannelNotOpenError,
  // Re-exported types that appear in the public API:
  type PeerConnectionOptions,
  type RtcOptions,
  type RtcPeerLike,
  type RtcPeerFactory,
  type Logger,
  type RoomState,
  type CreateRoomResult,
  type JoinRoomResult,
  type RejoinResult,
  type PeerStatus,
  type PeerConnectionEvents,
  type PeerDestroyedReason,
  type MediaDiagnostics,
} from './peer-connection.js';

// The WebRTC engine. Most applications only need `PeerConnection`, but the
// engine is usable standalone and its types appear in the wrapper's API.
export {
  RtcPeer,
  DataChannelHandle,
  hasWebRtcSupport,
  parseSignalData,
  signalFrame,
  SIGNAL_VERSION,
  type RtcPeerOptions,
  type RtcPeerEvents,
  type DataChannelSpec,
  type DataChannelEvents,
  type ResolvedChannelSpec,
  type WhenClosedPolicy,
  type ChannelDiagnostics,
  type ChannelMessage,
  type RtcEnv,
} from './rtc/index.js';

export {
  type RoomSession,
  type SessionStore,
  RoomSessionStore,
  BrowserSessionStore,
  MemorySessionStore,
} from './storage.js';

export {
  type ClientMessage,
  type ServerMessage,
  type Role,
  type CreateRoomMessage,
  type JoinRoomMessage,
  type RejoinRoomMessage,
  type SignalMessage,
  type PeerConnectedMessage,
  type CloseRoomMessage,
  type CreateRoomResponse,
  type JoinRoomResponse,
  type RejoinRoomResponse,
  type SignalResponseMessage,
  type GuestJoinedEvent,
  type PeerDisconnectedEvent,
  type PeerRejoinedEvent,
  type PeerResetEvent,
  type RoomIdleCloseEvent,
  type RoomClosedEvent,
  type RoomExpiredEvent,
  type ServerShutdownEvent,
  type ErrorResponseMessage,
  type SignalData,
  type SignalPayload,
  CloseCode,
  ErrorCode,
} from './types.js';

export { Transport, type WebSocketFactory, type WebSocketLike, parseFrame } from './transport.js';

export {
  generateEpoch,
  generateRequestId,
  base64url,
  randomBytes,
  fullJitterBackoff,
  SequenceCounter,
  isPlainObject,
  MAX_EPOCH_CHARS,
} from './util.js';
