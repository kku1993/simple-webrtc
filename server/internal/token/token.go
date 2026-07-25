// Package token implements the signed, self-describing rejoin token described
// in docs/DESIGN.md §"Rejoin tokens".
//
// Format:  <version>.<base64url(payload)>.<base64url(signature)>
// where signature = HMAC-SHA256(serverSecret, "<version>.<base64url(payload)>")
// and version is the literal "v1".
//
// Verification MUST happen before any payload field is trusted, and signature
// comparison MUST be constant-time.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Version is the current token format version.
const Version = "v1"

// Payload is the token payload. It is sufficient to reconstruct a room.
type Payload struct {
	V                 int    `json:"v"`
	RoomID            string `json:"roomId"`
	Role              string `json:"role"` // "host" or "guest"
	CreatedAt         int64  `json:"createdAt"` // unix seconds
	GuestPasswordHash string `json:"guestPasswordHash,omitempty"` // base64
	GuestPasswordSalt string `json:"guestPasswordSalt,omitempty"` // base64
}

// Signer signs and verifies rejoin tokens using a server secret.
type Signer struct {
	secret []byte
}

// NewSigner constructs a Signer. The secret must be at least 32 bytes; this is
// enforced by the config layer, but we double-check here so the token package
// is safe to use standalone.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < 32 {
		return nil, errors.New("server secret must be at least 32 bytes")
	}
	s := &Signer{secret: append([]byte(nil), secret...)}
	return s, nil
}

// Issue produces a signed token string for the given payload.
func (s *Signer) Issue(p Payload) (string, error) {
	if p.V == 0 {
		p.V = 1
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(body)
	signingInput := Version + "." + payloadB64
	sig := s.computeMAC(signingInput)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigB64, nil
}

// Verify parses a token, checks its signature in constant time, and returns the
// payload. It does NOT check the TTL or tombstones — those are policy concerns
// for the caller (see room.Rejoin).
func (s *Signer) Verify(tok string) (Payload, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != Version {
		return Payload{}, ErrInvalid
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Payload{}, ErrInvalid
	}
	want := s.computeMAC(signingInput)
	if !hmac.Equal(sig, want) {
		return Payload{}, ErrInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Payload{}, ErrInvalid
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, ErrInvalid
	}
	if p.V != 1 || (p.Role != "host" && p.Role != "guest") || p.RoomID == "" {
		return Payload{}, ErrInvalid
	}
	return p, nil
}

func (s *Signer) computeMAC(signingInput string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(signingInput))
	return m.Sum(nil)
}

// ErrInvalid is returned by Verify for any malformed or bad-signature token.
var ErrInvalid = errors.New("invalid rejoin token")

// Expired reports whether the token is older than ttl as of now.
func Expired(p Payload, now time.Time, ttl time.Duration) bool {
	created := time.Unix(p.CreatedAt, 0)
	return now.Sub(created) > ttl
}
