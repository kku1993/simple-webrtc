// Package roomid generates and validates room IDs following
// docs/ROOM_ID_SPEC.md (mirrored from the mahjong-p2p project).
//
// Room IDs have the form:
//
//	[shard][nid]
//
// — 6 lowercase characters, no separators. For example `ta0000`.
//
//   - shard is a single alphabetic Crockford base32 character
//     (digits [0-9] are reserved for future use). It identifies the backend
//     shard so a future load balancer can route by the first character.
//   - nid is 5 characters: the first is a Crockford base32 digit and the
//     remaining four are base 10 (0-9). Only one base32 digit is used to
//     avoid accidentally spelling a bad word.
//
// On input, Crockford base32 fuzzy decoding rules apply (O→0, I→1, L→1,
// case-insensitive); the canonical form is always lowercase. See
// https://www.crockford.com/base32.html
//
// The whole ID uses only [0-9a-z], a subset of the protocol's allowed
// roomId character set [0-9a-z_-].
package roomid

import (
	"crypto/rand"
	"errors"
	"strings"
)

// Crockford base32 alphabet (excludes I, L, O, U).
const base32Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Alphabetic-only Crockford base32 chars (no digits) — the only valid shard
// characters. Digits are reserved for future use per the room ID spec.
const base32Alphabetic = "ABCDEFGHJKMNPQRSTVWXYZ"

// NIDLength is the length of the nid portion: 1 base32 digit + 4 base-10
// digits.
const NIDLength = 5

// RoomIDLength is the total room id length: 1 shard char + nid.
const RoomIDLength = 1 + NIDLength

// MaxRetries is the number of collision retries Generate performs before
// giving up, matching the room ID spec's "retry up to 5 times" guidance.
const MaxRetries = 5

// ErrInvalidShard is returned by Generate when the supplied shard is not a
// single alphabetic Crockford base32 character.
var ErrInvalidShard = errors.New("roomid: shard must be a single alphabetic base32 character")

// DecodeBase32Char decodes a single character via Crockford base32 fuzzy
// rules. It returns the canonical uppercase symbol, or false if the
// character is not a valid base32 symbol.
func DecodeBase32Char(ch byte) (string, bool) {
	upper := strings.ToUpper(string(ch))
	switch upper {
	case "O":
		return "0", true
	case "I", "L":
		return "1", true
	}
	if strings.ContainsRune(base32Alphabet, rune(upper[0])) {
		return upper, true
	}
	return "", false
}

// decodeShardChar decodes a shard character and reports whether it is a
// valid alphabetic shard char (digits are reserved for future use). It
// returns the canonical lowercase symbol.
func decodeShardChar(ch byte) (string, bool) {
	decoded, ok := DecodeBase32Char(ch)
	if !ok || !strings.ContainsRune(base32Alphabetic, rune(decoded[0])) {
		return "", false
	}
	return strings.ToLower(decoded), true
}

// IsValidShardName reports whether shard is a single alphabetic Crockford
// base32 character. Digits are rejected because they are reserved for future
// use per the room ID spec.
func IsValidShardName(shard string) bool {
	if len(shard) != 1 {
		return false
	}
	_, ok := decodeShardChar(shard[0])
	return ok
}

// ParsedRoomID is the result of parsing a room id into its shard and nid
// components, both in canonical lowercase form.
type ParsedRoomID struct {
	Shard string
	NID   string
}

// ParseRoomID parses a room id with Crockford base32 fuzzy decoding. It
// returns false for malformed ids. The returned shard and nid are in
// canonical lowercase form.
func ParseRoomID(id string) (ParsedRoomID, bool) {
	if len(id) != RoomIDLength {
		return ParsedRoomID{}, false
	}
	shardLower, ok := decodeShardChar(id[0])
	if !ok {
		return ParsedRoomID{}, false
	}
	nidFirstDecoded, ok := DecodeBase32Char(id[1])
	if !ok {
		return ParsedRoomID{}, false
	}
	// Remaining 4 nid digits are base 10 (0-9), with Crockford fuzzy
	// decoding mapping O→0, I→1, L→1.
	nid := strings.ToLower(nidFirstDecoded)
	for i := 2; i < RoomIDLength; i++ {
		decoded, ok := DecodeBase32Char(id[i])
		if !ok || len(decoded) != 1 || decoded[0] < '0' || decoded[0] > '9' {
			return ParsedRoomID{}, false
		}
		nid += strings.ToLower(decoded)
	}
	return ParsedRoomID{Shard: shardLower, NID: nid}, true
}

// IsValidRoomID reports whether id is a well-formed room id belonging to
// expectedShard. Both id and expectedShard are decoded with Crockford base32
// fuzzy rules before comparison.
func IsValidRoomID(id, expectedShard string) bool {
	parsed, ok := ParseRoomID(id)
	if !ok {
		return false
	}
	if !IsValidShardName(expectedShard) {
		return false
	}
	expectedLower, _ := decodeShardChar(expectedShard[0])
	return parsed.Shard == expectedLower
}

// NormalizeRoomID returns the canonical lowercase form of id, applying
// Crockford base32 fuzzy decoding (O→0, I→1, L→1, case-insensitive). It
// returns false if id is not a valid room id. Callers must use the returned
// canonical form for all map lookups and storage keys so that uppercase or
// fuzzy-equivalent input (e.g. "TA0000", "tO0000") resolves to the same room
// as the canonical form ("ta0000", "t00000") — see docs/ROOM_ID_SPEC.md
// §"Backend handling".
func NormalizeRoomID(id string) (string, bool) {
	parsed, ok := ParseRoomID(id)
	if !ok {
		return "", false
	}
	return parsed.Shard + parsed.NID, true
}

// Generate returns a new room id of the form [shard][nid] for the given
// shard. The shard must be a single alphabetic Crockford base32 character
// (use IsValidShardName to check); it is lowercased in the output.
//
// exists is called for each candidate and must report whether the id is
// already in use (e.g. live room or live tombstone). Generate retries with a
// fresh candidate on collision, up to MaxRetries times.
func Generate(shard string, exists func(id string) bool) (string, error) {
	if !IsValidShardName(shard) {
		return "", ErrInvalidShard
	}
	if exists == nil {
		return "", errors.New("roomid: exists callback is nil")
	}
	shardLower := strings.ToLower(string(shard[0]))
	for i := 0; i < MaxRetries; i++ {
		id, err := candidate(shardLower)
		if err != nil {
			return "", err
		}
		if !exists(id) {
			return id, nil
		}
	}
	return "", errors.New("roomid: could not generate unique room id after retries")
}

// candidate builds a single [shard][nid] string. The nid is 1 random base32
// digit + 4 random base-10 digits, drawn from crypto/rand.
func candidate(shardLower string) (string, error) {
	nidFirst, err := randomBase32Char()
	if err != nil {
		return "", err
	}
	rest, err := randomDecimalDigits(4)
	if err != nil {
		return "", err
	}
	return shardLower + nidFirst + rest, nil
}

// randomBase32Char returns one random Crockford base32 character (lowercase),
// drawn from crypto/rand. The full alphabet (including digits) is used for
// the nid's first position per the room ID spec.
func randomBase32Char() (string, error) {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// 256 % 32 == 0, so there is no modulo bias over the 32-symbol alphabet.
	idx := int(b[0]) % len(base32Alphabet)
	return strings.ToLower(string(base32Alphabet[idx])), nil
}

// randomDecimalDigits returns n random base-10 digits drawn from crypto/rand.
func randomDecimalDigits(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	// 256 % 10 == 6, so digits 0-5 are very slightly over-represented. For
	// a 4-digit collision-absorbing suffix this bias is negligible and well
	// below the cost of rejection sampling.
	for i, b := range buf {
		buf[i] = byte('0' + int(b)%10)
	}
	return string(buf), nil
}
