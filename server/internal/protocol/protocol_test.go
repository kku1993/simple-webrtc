package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRoleOther(t *testing.T) {
	if RoleHost.Other() != RoleGuest {
		t.Errorf("host.Other = %s, want guest", RoleHost.Other())
	}
	if RoleGuest.Other() != RoleHost {
		t.Errorf("guest.Other = %s, want host", RoleGuest.Other())
	}
}

func TestRoleIsValid(t *testing.T) {
	if !RoleHost.IsValid() || !RoleGuest.IsValid() {
		t.Errorf("host/guest should be valid")
	}
	if Role("admin").IsValid() {
		t.Errorf("admin should be invalid")
	}
}

func TestErrorCodeRetryable(t *testing.T) {
	retryable := []ErrorCode{
		ErrHandshakeTimeout, ErrRoomNotFound, ErrTurnstileRequired,
		ErrTurnstileInvalid, ErrRateLimited, ErrServerAtCapacity,
		ErrSignalBufferOverflow,
	}
	for _, c := range retryable {
		if !c.retryable() {
			t.Errorf("%d should be retryable", c)
		}
	}
	notRetryable := []ErrorCode{
		ErrMalformedMessage, ErrUnknownMessageType, ErrPayloadTooLarge,
		ErrUnexpectedState, ErrRoomFull, ErrRoomClosed, ErrRoomExpired,
		ErrInvalidGuestPassword, ErrTooManyPasswordAttempts,
		ErrInvalidRejoinToken, ErrOriginNotAllowed, ErrUnsupportedProtocolVersion,
	}
	for _, c := range notRetryable {
		if c.retryable() {
			t.Errorf("%d should NOT be retryable", c)
		}
	}
}

func TestNewErrorSetsFields(t *testing.T) {
	retry := 500
	e := NewError(ErrRateLimited, "slow down", "req-1", &retry)
	if e.Type != TypeErrorResponse {
		t.Errorf("Type = %s", e.Type)
	}
	if e.ErrorCode != ErrRateLimited {
		t.Errorf("ErrorCode = %d", e.ErrorCode)
	}
	if !e.Retryable {
		t.Errorf("Retryable should be true")
	}
	if e.RetryAfterMs == nil || *e.RetryAfterMs != 500 {
		t.Errorf("RetryAfterMs = %v", e.RetryAfterMs)
	}
	if e.RequestID != "req-1" {
		t.Errorf("RequestID = %s", e.RequestID)
	}
}

// The relay path hand-writes signal-response instead of marshaling the struct,
// so the two encoders have to agree byte for byte -- including encoding/json's
// HTML escaping, which a naive hand-rolled encoder silently drops.
func TestAppendSignalResponseMatchesMarshal(t *testing.T) {
	payloads := []string{
		`{"type":"offer","sdp":"v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"}`,
		"",
		"plain",
		`quotes " and backslash \ and slash /`,
		"control\x00\x1f\n\t chars",
		"html < > & chars",
		"unicode ünïcödé 🎉 and \u2028\u2029",
	}
	epochs := []string{"h1", `weird "epoch"`, "<script>", "épok"}

	for _, p := range payloads {
		for _, epoch := range epochs {
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			want, err := json.Marshal(SignalResponse{
				Type:       TypeSignalResponse,
				FromRole:   RoleHost,
				FromEpoch:  epoch,
				Seq:        42,
				Data:       p,
				ReceivedAt: "2026-08-13T01:02:03Z",
			})
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			got := AppendSignalResponse(nil, RoleHost, epoch, 42, data, "2026-08-13T01:02:03Z")
			if string(got) != string(want) {
				t.Errorf("payload %q epoch %q:\n got %s\nwant %s", p, epoch, got, want)
			}
		}
	}
}

// A missing `data` used to decode to the empty string; it still relays as one.
func TestAppendSignalResponseNilData(t *testing.T) {
	got := AppendSignalResponse(nil, RoleGuest, "g1", 1, nil, "2026-08-13T01:02:03Z")
	var back SignalResponse
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", got, err)
	}
	if back.Data != "" || back.FromRole != RoleGuest || back.Seq != 1 {
		t.Errorf("got %+v", back)
	}
}

// AppendSignalResponse appends, so a caller can build into a buffer it owns.
func TestAppendSignalResponseAppends(t *testing.T) {
	dst := []byte("prefix:")
	got := AppendSignalResponse(dst, RoleHost, "h", 1, json.RawMessage(`"x"`), "t")
	if !strings.HasPrefix(string(got), "prefix:{") {
		t.Errorf("did not append to dst: %s", got)
	}
}

func TestIsJSONString(t *testing.T) {
	cases := map[string]bool{`"a"`: true, `""`: true, `5`: false, `{"a":1}`: false, `null`: false, ``: false}
	for in, want := range cases {
		if got := IsJSONString(json.RawMessage(in)); got != want {
			t.Errorf("IsJSONString(%q) = %v, want %v", in, got, want)
		}
	}
	if IsJSONString(nil) {
		t.Errorf("IsJSONString(nil) = true, want false")
	}
}
