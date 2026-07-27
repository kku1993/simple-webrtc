// Protocol message types for the simple-peer signaling server.
//
// Mirrors docs/DESIGN.md. All client→server and server→client frames are JSON
// objects exchanged over a single WebSocket. Identifiers (except roomId) use
// base64url encoding.

export type Role = 'host' | 'guest';

/** Common envelope fields for every client→server message. */
export interface ClientMessageBase {
  type: string;
  requestId?: string;
}

/** Common envelope fields for every server→client message. */
export interface ServerMessageBase {
  type: string;
  requestId?: string;
}

// ---------------------------------------------------------------------------
// Client → server
// ---------------------------------------------------------------------------

export interface CreateRoomMessage extends ClientMessageBase {
  type: 'create-room';
  hostEpoch: string;
  guestPassword?: string;
  cloudflareTurnstileToken?: string;
}

export interface JoinRoomMessage extends ClientMessageBase {
  type: 'join-room';
  roomId: string;
  guestEpoch: string;
  guestPassword?: string;
}

export interface RejoinRoomMessage extends ClientMessageBase {
  type: 'rejoin-room';
  rejoinToken: string;
  epoch: string;
}

export interface SignalMessage extends ClientMessageBase {
  type: 'signal';
  seq: number;
  data: string;
}

export interface PeerConnectedMessage extends ClientMessageBase {
  type: 'peer-connected';
}

export interface CloseRoomMessage extends ClientMessageBase {
  type: 'close-room';
}

export type ClientMessage =
  | CreateRoomMessage
  | JoinRoomMessage
  | RejoinRoomMessage
  | SignalMessage
  | PeerConnectedMessage
  | CloseRoomMessage;

// ---------------------------------------------------------------------------
// Server → client
// ---------------------------------------------------------------------------

export interface CreateRoomResponse extends ServerMessageBase {
  type: 'create-room-response';
  roomId: string;
  role: 'host';
  rejoinToken: string;
  peerDeadlineAt: string;
  peerDeadlineInSeconds: number;
  roomExpiresAt: string;
  roomExpiresInSeconds: number;
  rejoinTokenExpiresAt: string;
}

export interface JoinRoomResponse extends ServerMessageBase {
  type: 'join-room-response';
  roomId: string;
  role: 'guest';
  rejoinToken: string;
  hostConnected: boolean;
  hostEpoch: string | null;
  guestEpoch: string;
  roomExpiresAt: string;
  roomExpiresInSeconds: number;
  rejoinTokenExpiresAt: string;
}

export interface RejoinRoomResponse extends ServerMessageBase {
  type: 'rejoin-room-response';
  roomId: string;
  role: Role;
  recreated: boolean;
  peerConnected: boolean;
  hostEpoch: string | null;
  guestEpoch: string | null;
  peerDeadlineAt: string | null;
  peerDeadlineInSeconds: number | null;
  roomExpiresAt: string;
  roomExpiresInSeconds: number;
  rejoinTokenExpiresAt: string;
}

export interface SignalResponseMessage extends ServerMessageBase {
  type: 'signal-response';
  fromRole: Role;
  fromEpoch: string;
  seq: number;
  data: string;
  receivedAt: string;
}

export interface GuestJoinedEvent extends ServerMessageBase {
  type: 'guest-joined';
  guestEpoch: string;
}

export interface PeerDisconnectedEvent extends ServerMessageBase {
  type: 'peer-disconnected';
  role: Role;
}

export interface PeerRejoinedEvent extends ServerMessageBase {
  type: 'peer-rejoined';
  role: Role;
  epoch: string;
}

export interface PeerResetEvent extends ServerMessageBase {
  type: 'peer-reset';
  role: Role;
  epoch: string;
}

export interface RoomIdleCloseEvent extends ServerMessageBase {
  type: 'room-idle-close';
  reason: 'peer-connected';
}

export interface RoomClosedEvent extends ServerMessageBase {
  type: 'room-closed';
  reason: string;
}

export interface RoomExpiredEvent extends ServerMessageBase {
  type: 'room-expired';
}

export interface ServerShutdownEvent extends ServerMessageBase {
  type: 'server-shutdown';
  reconnectAfterMs: number;
}

export interface ErrorResponseMessage extends ServerMessageBase {
  type: 'error-response';
  errorCode: number;
  message: string;
  retryable: boolean;
  retryAfterMs?: number;
}

export type ServerMessage =
  | CreateRoomResponse
  | JoinRoomResponse
  | RejoinRoomResponse
  | SignalResponseMessage
  | GuestJoinedEvent
  | PeerDisconnectedEvent
  | PeerRejoinedEvent
  | PeerResetEvent
  | RoomIdleCloseEvent
  | RoomClosedEvent
  | RoomExpiredEvent
  | ServerShutdownEvent
  | ErrorResponseMessage;

// ---------------------------------------------------------------------------
// Close codes
// ---------------------------------------------------------------------------

export enum CloseCode {
  /** Protocol error / handshake timeout. */
  PROTOCOL_ERROR = 4001,
  /** Policy violation (origin, oversized frame). */
  POLICY_VIOLATION = 4003,
  /** Rate limited. */
  RATE_LIMITED = 4008,
  /** Room closed (terminal). */
  ROOM_CLOSED = 4013,
  /** Room expired, or peer deadline reached — rejoin with token if still valid. */
  ROOM_EXPIRED = 4014,
  /** Released after peer connection — SUCCESS, do not reconnect. */
  RELEASED_AFTER_PEER_CONNECTED = 4200,
  /** Server shutting down — reconnect after reconnectAfterMs with jitter. */
  SERVER_SHUTTING_DOWN = 4300,
  /** Replaced by a newer connection for this slot — do not reconnect. */
  REPLACED = 4400,
}

// ---------------------------------------------------------------------------
// Error codes (subset that the client cares about; see DESIGN.md error table).
// ---------------------------------------------------------------------------

export enum ErrorCode {
  MALFORMED_MESSAGE = 1001,
  UNKNOWN_MESSAGE_TYPE = 1002,
  PAYLOAD_TOO_LARGE = 1003,
  UNEXPECTED_STATE = 1004,
  HANDSHAKE_TIMEOUT = 1005,
  ROOM_NOT_FOUND = 1101,
  ROOM_FULL = 1103,
  ROOM_CLOSED = 1104,
  ROOM_EXPIRED = 1105,
  INVALID_GUEST_PASSWORD = 1201,
  TOO_MANY_PASSWORD_ATTEMPTS = 1202,
  INVALID_REJOIN_TOKEN = 1203,
  TURNSTILE_REQUIRED = 1204,
  TURNSTILE_INVALID = 1205,
  RATE_LIMITED = 1301,
  SERVER_AT_CAPACITY = 1302,
  SIGNAL_BUFFER_OVERFLOW = 1303,
  ORIGIN_NOT_ALLOWED = 1401,
  UNSUPPORTED_PROTOCOL_VERSION = 1402,
}

// ---------------------------------------------------------------------------
// Peer-to-peer signal framing
//
// Renegotiation after the server releases the sockets travels over the engine's
// dedicated control channel rather than the application's data channel. See
// `src/rtc/signal.ts` for the frame format and DESIGN.md "Renegotiation after
// socket release" for when it is used.
// ---------------------------------------------------------------------------

export type { SignalData, SignalPayload } from './rtc/signal.js';
