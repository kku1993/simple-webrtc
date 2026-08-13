// Package protocol defines the wire format: message types, error codes, and
// WebSocket close codes used by the signaling server.
//
// All client→server and server→client messages are JSON objects with a `type`
// field. This package mirrors the message tables in docs/DESIGN.md.
package protocol

import (
	"encoding/json"
	"strconv"
)

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
//
// Data is kept as the raw JSON token the client sent — a quoted string — and
// is never decoded. The server treats the signaling payload as opaque, so
// unquoting it on the way in only to re-quote it on the way out is pure cost,
// and it is the largest field in the message: clients send
// `JSON.stringify(sdp)`, so nearly every byte of it is an escaped quote. See
// AppendSignalResponse.
type SignalMsg struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Seq       int             `json:"seq"`
	Data      json.RawMessage `json:"data"`
}

// IsJSONString reports whether raw is a JSON string token. Decoding into a
// json.RawMessage accepts any JSON value, so the type check that
// `Data string` used to get from the decoder has to be made explicitly.
// An absent field (nil) is not a string; callers decide whether that is an
// error or an empty payload.
func IsJSONString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
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

// SignalResponse documents the server→client signal-response shape. It is not
// used to encode one: AppendSignalResponse writes the same JSON directly so
// the payload can be relayed without a decode/re-encode round trip. Keep the
// two in sync — the field order here is the field order on the wire.
type SignalResponse struct {
	Type       string `json:"type"`
	FromRole   Role   `json:"fromRole"`
	FromEpoch  string `json:"fromEpoch"`
	Seq        int    `json:"seq"`
	Data       string `json:"data"`
	ReceivedAt string `json:"receivedAt"`
}

// AppendSignalResponse appends a signal-response message to dst and returns
// the extended slice.
//
// data is the `data` token exactly as it arrived from the sender and is
// spliced in verbatim, which is the whole point: it is already a valid JSON
// string, so copying it is enough. Callers must have checked it with
// IsJSONString; a nil data is written as an empty string, matching what a
// missing field used to decode to.
//
// The output is byte-identical to json.Marshal of the equivalent
// SignalResponse for any data the sender could have produced, including its
// escaping of <, > and & — appendJSONString mirrors encoding/json.
func AppendSignalResponse(dst []byte, fromRole Role, fromEpoch string, seq int, data json.RawMessage, receivedAt string) []byte {
	dst = append(dst, `{"type":"`...)
	dst = append(dst, TypeSignalResponse...)
	dst = append(dst, `","fromRole":`...)
	dst = appendJSONString(dst, string(fromRole))
	dst = append(dst, `,"fromEpoch":`...)
	dst = appendJSONString(dst, fromEpoch)
	dst = append(dst, `,"seq":`...)
	dst = strconv.AppendInt(dst, int64(seq), 10)
	dst = append(dst, `,"data":`...)
	if len(data) == 0 {
		dst = append(dst, `""`...)
	} else {
		dst = append(dst, data...)
	}
	dst = append(dst, `,"receivedAt":`...)
	dst = appendJSONString(dst, receivedAt)
	return append(dst, '}')
}

// appendJSONString appends s as a JSON string literal, escaping exactly what
// encoding/json escapes with its default HTML-escaping encoder. Anything
// outside plain printable ASCII goes through encoding/json itself rather than
// being reimplemented here: the fields this is used for (roles, epochs,
// timestamps) are ASCII in every non-adversarial case, and correctness matters
// more than the speed of the exception.
func appendJSONString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			b, err := json.Marshal(s)
			if err != nil {
				return append(dst, `""`...)
			}
			return append(dst, b...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, s...)
	return append(dst, '"')
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
