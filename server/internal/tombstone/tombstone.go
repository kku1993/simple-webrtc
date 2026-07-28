// Package tombstone implements the bounded LRU of deliberately-closed room IDs
// described in docs/DESIGN.md §"Tombstones".
//
// Tombstones are written only for deliberate terminations (close-room, password
// lockout, admin closure). They are NOT written for room-expired or
// room-idle-close, both of which remain rejoinable. Storage is bounded by an
// LRU with a TTL; entries are also evicted on overflow.
package tombstone

import (
	"sync"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/lru"
)

// Reason is why a room was closed.
type Reason string

const (
	ReasonClosedByPeer    Reason = "closed-by-peer"
	ReasonPasswordLockout Reason = "password-lockout"
	ReasonAdmin           Reason = "admin"
)

// Entry records when and why a room was closed.
type Entry struct {
	ClosedAt time.Time
	Reason   Reason
}

// Store is a bounded, TTL-evicting tombstone map keyed by roomId.
type Store struct {
	mu      sync.Mutex
	c       *lru.Cache[Entry]
	ttl     time.Duration
	nowFunc func() time.Time
}

// New constructs a Store with the given capacity and TTL.
func New(capacity int, ttl time.Duration) *Store {
	return &Store{
		c:       lru.New[Entry](capacity),
		ttl:     ttl,
		nowFunc: time.Now,
	}
}

// SetWithClock installs a custom clock, primarily for tests.
func (s *Store) SetWithClock(now func() time.Time) { s.nowFunc = now }

// Add writes a tombstone for roomId. Existing entries are overwritten.
func (s *Store) Add(roomID string, reason Reason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowFunc()
	s.c.Put(roomID, Entry{ClosedAt: now, Reason: reason}, nil)
}

// Has reports whether an unexpired tombstone exists for roomId.
func (s *Store) Has(roomID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.c.Get(roomID)
	if !ok {
		return false
	}
	if s.nowFunc().Sub(e.ClosedAt) > s.ttl {
		// Expired; opportunistically remove.
		s.c.Delete(roomID)
		return false
	}
	return true
}

// Get returns the tombstone entry for roomId if present and unexpired.
func (s *Store) Get(roomID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.c.Get(roomID)
	if !ok {
		return Entry{}, false
	}
	if s.nowFunc().Sub(e.ClosedAt) > s.ttl {
		s.c.Delete(roomID)
		return Entry{}, false
	}
	return e, true
}

// Sweep removes all expired entries. Intended to be called periodically by the
// timer sweep goroutine.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowFunc()
	removed := 0
	var toDelete []string
	s.c.Range(func(key string, e Entry) bool {
		if now.Sub(e.ClosedAt) > s.ttl {
			toDelete = append(toDelete, key)
		}
		return true
	})
	for _, k := range toDelete {
		s.c.Delete(k)
		removed++
	}
	return removed
}

// Len returns the current number of entries (including possibly expired ones
// not yet swept).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Len()
}
