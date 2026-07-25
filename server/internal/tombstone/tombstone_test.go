package tombstone

import (
	"testing"
	"time"
)

func TestAddHas(t *testing.T) {
	s := New(100, time.Hour)
	s.Add("room1", ReasonClosedByPeer)
	if !s.Has("room1") {
		t.Errorf("expected tombstone for room1")
	}
	if s.Has("room2") {
		t.Errorf("did not expect tombstone for room2")
	}
}

func TestGetReturnsReason(t *testing.T) {
	s := New(100, time.Hour)
	s.Add("r", ReasonPasswordLockout)
	e, ok := s.Get("r")
	if !ok {
		t.Fatalf("expected entry")
	}
	if e.Reason != ReasonPasswordLockout {
		t.Errorf("reason = %q, want %q", e.Reason, ReasonPasswordLockout)
	}
}

func TestTTLExpiry(t *testing.T) {
	now := time.Now()
	s := New(100, time.Hour)
	s.SetWithClock(func() time.Time { return now })
	s.Add("r", ReasonClosedByPeer)
	s.SetWithClock(func() time.Time { return now.Add(2 * time.Hour) })
	if s.Has("r") {
		t.Errorf("expected tombstone to be expired")
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	now := time.Now()
	s := New(100, time.Hour)
	s.SetWithClock(func() time.Time { return now })
	s.Add("a", ReasonClosedByPeer)
	s.Add("b", ReasonClosedByPeer)
	s.SetWithClock(func() time.Time { return now.Add(2 * time.Hour) })
	s.Add("c", ReasonClosedByPeer)
	// a and b are expired; c is fresh.
	removed := s.Sweep()
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if !s.Has("c") {
		t.Errorf("c should remain")
	}
}

func TestEvictionOnOverflow(t *testing.T) {
	s := New(2, time.Hour)
	s.Add("a", ReasonClosedByPeer)
	s.Add("b", ReasonClosedByPeer)
	s.Has("a") // touch a so b becomes LRU
	s.Add("c", ReasonClosedByPeer)
	if s.Has("b") {
		t.Errorf("b should have been evicted")
	}
	if !s.Has("a") {
		t.Errorf("a should remain")
	}
}
