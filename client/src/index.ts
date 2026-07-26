// Public entry point for the peer-client package.
//
// A TypeScript client that wraps `simple-peer` and speaks the signaling
// protocol described in `docs/DESIGN.md`. See the README for usage.

export {
  PeerConnection,
  SignalingError,
  // Re-exported types that appear in the public API:
  type PeerConnectionOptions,
  type SimplePeerOptions,
  type PeerLike,
  type SimplePeerFactory,
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
  type RenegotiateFrame,
  type DataChannelFrame,
  type SimplePeer,
  type SimplePeerInstance,
  type SimplePeerSignalData,
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
