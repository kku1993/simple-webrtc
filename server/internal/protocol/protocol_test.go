package protocol

import "testing"

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
