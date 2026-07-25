// Package protocol defines the wire format: message types, error codes, and
// WebSocket close codes used by the signaling server.
//
// All client→server and server→client messages are JSON objects with a `type`
// field. This package mirrors the message tables in docs/DESIGN.md.
package protocol

import "encoding/json"

// Role identifies a slot within a room.
type Role string

const (
	RoleHost  Role = "host"
	RoleGuest Role = "guest"
)

// IsValid reports whether r is a recognized role.
func (r Role) IsValid() bool { return r == RoleHost || r == RoleGuest }

// Other returns the opposite role. Behaviour is undefined for invalid roles.
func (r Role) Other() Role {
	if r == RoleHost {
		return RoleGuest
	}
	return RoleHost
}

// ErrorCode is a numeric error code as defined in the design doc.
type ErrorCode int

const (
	ErrMalformedMessage          ErrorCode = 1001
	ErrUnknownMessageType        ErrorCode = 1002
	ErrPayloadTooLarge           ErrorCode = 1003
	ErrUnexpectedState           ErrorCode = 1004
	ErrHandshakeTimeout          ErrorCode = 1005
	ErrRoomNotFound              ErrorCode = 1101
	ErrRoomFull                  ErrorCode = 1103
	ErrRoomClosed                ErrorCode = 1104
	ErrRoomExpired               ErrorCode = 1105
	ErrInvalidGuestPassword      ErrorCode = 1201
	ErrTooManyPasswordAttempts   ErrorCode = 1202
	ErrInvalidRejoinToken        ErrorCode = 1203
	ErrTurnstileRequired         ErrorCode = 1204
	ErrTurnstileInvalid          ErrorCode = 1205
	ErrRateLimited               ErrorCode = 1301
	ErrServerAtCapacity          ErrorCode = 1302
	ErrSignalBufferOverflow      ErrorCode = 1303
	ErrOriginNotAllowed          ErrorCode = 1401
	ErrUnsupportedProtocolVersion ErrorCode = 1402
)

// retryable is the reference retry policy from the design doc's error table.
// Operators should not change this without updating the doc.
func (c ErrorCode) retryable() bool {
	switch c {
	case ErrHandshakeTimeout,
		ErrRoomNotFound,
		ErrTurnstileRequired,
		ErrTurnstileInvalid,
		ErrRateLimited,
		ErrServerAtCapacity,
		ErrSignalBufferOverflow:
		return true
	default:
		return false
	}
}

// CloseCode is a WebSocket close code as defined in the design doc.
type CloseCode int

const (
	CloseProtocolError       CloseCode = 4001
	ClosePolicyViolation     CloseCode = 4003
	CloseRateLimited         CloseCode = 4008
	CloseRoomClosed          CloseCode = 4013
	CloseRoomExpired         CloseCode = 4014
	ClosePeerConnected       CloseCode = 4200
	CloseServerShutdown      CloseCode = 4300
	CloseReplaced            CloseCode = 4400
)

// Inbound message types (client → server).
const (
	TypeCreateRoom    = "create-room"
	TypeJoinRoom      = "join-room"
	TypeRejoinRoom    = "rejoin-room"
	TypeSignal        = "signal"
	TypePeerConnected = "peer-connected"
	TypeCloseRoom     = "close-room"
)

// Outbound message types (server → client).
const (
	TypeCreateRoomResponse  = "create-room-response"
	TypeJoinRoomResponse    = "join-room-response"
	TypeRejoinRoomResponse  = "rejoin-room-response"
	TypeSignalResponse      = "signal-response"
	TypeGuestJoined         = "guest-joined"
	TypePeerDisconnected    = "peer-disconnected"
	TypePeerRejoined        = "peer-rejoined"
	TypePeerReset           = "peer-reset"
	TypeRoomIdleClose       = "room-idle-close"
	TypeRoomClosed          = "room-closed"
	TypeRoomExpired         = "room-expired"
	TypeServerShutdown      = "server-shutdown"
	TypeErrorResponse       = "error-response"
)

// Envelope is the common shape of every client→server message.
type Envelope struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// CreateRoomMsg is the client→server create-room message.
type CreateRoomMsg struct {
	Type                     string `json:"type"`
	RequestID                string `json:"requestId,omitempty"`
	HostEpoch                string `json:"hostEpoch"`
	GuestPassword            string `json:"guestPassword,omitempty"`
	CloudflareTurnstileToken string `json:"cloudflareTurnstileToken,omitempty"`
}

// JoinRoomMsg is the client→server join-room message.
type JoinRoomMsg struct {
	Type         string `json:"type"`
	RequestID    string `json:"requestId,omitempty"`
	RoomID       string `json:"roomId"`
	GuestEpoch   string `json:"guestEpoch"`
	GuestPassword string `json:"guestPassword,omitempty"`
}

// RejoinRoomMsg is the client→server rejoin-room message.
type RejoinRoomMsg struct {
	Type       string `json:"type"`
	RequestID  string `json:"requestId,omitempty"`
	RejoinToken string `json:"rejoinToken"`
	Epoch      string `json:"epoch"`
}

// SignalMsg is the client→server signal message.
type SignalMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
	Seq       int    `json:"seq"`
	Data      string `json:"data"`
}

// PeerConnectedMsg is the client→server peer-connected message.
type PeerConnectedMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
}

// CloseRoomMsg is the client→server close-room message.
type CloseRoomMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
}

// --- Server → client messages ---

type CreateRoomResponse struct {
	Type                    string `json:"type"`
	RequestID               string `json:"requestId,omitempty"`
	RoomID                  string `json:"roomId"`
	Role                    Role   `json:"role"`
	RejoinToken             string `json:"rejoinToken"`
	PeerDeadlineAt          string `json:"peerDeadlineAt"`
	PeerDeadlineInSeconds   int    `json:"peerDeadlineInSeconds"`
	RoomExpiresAt           string `json:"roomExpiresAt"`
	RoomExpiresInSeconds    int    `json:"roomExpiresInSeconds"`
	RejoinTokenExpiresAt    string `json:"rejoinTokenExpiresAt"`
}

type JoinRoomResponse struct {
	Type                  string  `json:"type"`
	RequestID             string  `json:"requestId,omitempty"`
	RoomID                string  `json:"roomId"`
	Role                  Role    `json:"role"`
	RejoinToken           string  `json:"rejoinToken"`
	HostConnected         bool    `json:"hostConnected"`
	HostEpoch             *string `json:"hostEpoch"`
	GuestEpoch            string  `json:"guestEpoch"`
	RoomExpiresAt         string  `json:"roomExpiresAt"`
	RoomExpiresInSeconds  int     `json:"roomExpiresInSeconds"`
	RejoinTokenExpiresAt  string  `json:"rejoinTokenExpiresAt"`
}

type RejoinRoomResponse struct {
	Type                    string  `json:"type"`
	RequestID               string  `json:"requestId,omitempty"`
	RoomID                  string  `json:"roomId"`
	Role                    Role    `json:"role"`
	Recreated               bool    `json:"recreated"`
	PeerConnected           bool    `json:"peerConnected"`
	HostEpoch               *string `json:"hostEpoch"`
	GuestEpoch              *string `json:"guestEpoch"`
	PeerDeadlineAt          *string `json:"peerDeadlineAt"`
	PeerDeadlineInSeconds   *int    `json:"peerDeadlineInSeconds"`
	RoomExpiresAt           string  `json:"roomExpiresAt"`
	RoomExpiresInSeconds    int     `json:"roomExpiresInSeconds"`
	RejoinTokenExpiresAt    string  `json:"rejoinTokenExpiresAt"`
}

type SignalResponse struct {
	Type        string `json:"type"`
	FromRole    Role   `json:"fromRole"`
	FromEpoch   string `json:"fromEpoch"`
	Seq         int    `json:"seq"`
	Data        string `json:"data"`
	ReceivedAt  string `json:"receivedAt"`
}

type GuestJoinedEvent struct {
	Type       string `json:"type"`
	GuestEpoch string `json:"guestEpoch"`
}

type PeerDisconnectedEvent struct {
	Type string `json:"type"`
	Role Role   `json:"role"`
}

type PeerRejoinedEvent struct {
	Type  string `json:"type"`
	Role  Role   `json:"role"`
	Epoch string `json:"epoch"`
}

type PeerResetEvent struct {
	Type  string `json:"type"`
	Role  Role   `json:"role"`
	Epoch string `json:"epoch"`
}

type RoomIdleCloseEvent struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type RoomClosedEvent struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type RoomExpiredEvent struct {
	Type string `json:"type"`
}

type ServerShutdownEvent struct {
	Type             string `json:"type"`
	ReconnectAfterMs int    `json:"reconnectAfterMs"`
}

// ErrorResponse is the server→client error envelope.
type ErrorResponse struct {
	Type         string `json:"type"`
	RequestID    string `json:"requestId,omitempty"`
	ErrorCode    ErrorCode `json:"errorCode"`
	Message      string    `json:"message"`
	Retryable    bool      `json:"retryable"`
	RetryAfterMs *int      `json:"retryAfterMs,omitempty"`
}

// NewError constructs an ErrorResponse with the retryable flag preset from the
// reference table and an optional retryAfterMs.
func NewError(code ErrorCode, msg string, requestID string, retryAfterMs *int) ErrorResponse {
	return ErrorResponse{
		Type:         TypeErrorResponse,
		RequestID:    requestID,
		ErrorCode:    code,
		Message:      msg,
		Retryable:    code.retryable(),
		RetryAfterMs: retryAfterMs,
	}
}
