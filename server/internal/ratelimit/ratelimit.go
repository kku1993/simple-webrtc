// Package ratelimit implements the in-memory token bucket rate limiters
// described in docs/DESIGN.md §"Rate limits".
//
// Two scopes are supported:
//   - Per-IP limiters, kept in a bounded LRU so the per-IP map cannot itself be
//     a memory-exhaustion vector.
//   - Per-IP room counters, also bounded LRU.
//   - Global counters (rooms, connections) enforced by simple atomic counters.
//
// All limiters are in-memory and valid only for the lifetime of the process.
package ratelimit

import (
	"sync"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/lru"
)

// Bucket is a token bucket. It is not safe for concurrent use by itself; the
// surrounding Map guards it.
type Bucket struct {
	tokens     float64
	last       time.Time
	ratePerSec float64 // refill rate
	burst      float64
}

// NewBucket creates a bucket pre-filled with `burst` tokens, refilling at
// ratePerSec tokens per second.
func NewBucket(ratePerSec, burst float64, now time.Time) *Bucket {
	return &Bucket{tokens: burst, last: now, ratePerSec: ratePerSec, burst: burst}
}

// Take attempts to consume n tokens at time `now`. It returns true on success.
// On failure, it returns the time the caller should wait before retrying.
func (b *Bucket) Take(n float64, now time.Time) (bool, time.Duration) {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.ratePerSec
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	needed := n - b.tokens
	wait := time.Duration(needed / b.ratePerSec * float64(time.Second))
	if wait <= 0 {
		wait = time.Millisecond
	}
	return false, wait
}

// Map is a bounded LRU of per-key token buckets.
type Map struct {
	mu         sync.Mutex
	c          *lru.Cache[*Bucket]
	ratePerSec float64
	burst      float64
	nowFunc    func() time.Time
}

// NewMap creates a per-key bucket map with the given capacity and per-bucket
// rate (tokens per second) and burst (maximum tokens).
func NewMap(capacity int, ratePerSec, burst float64) *Map {
	return &Map{
		c:          lru.New[*Bucket](capacity),
		ratePerSec: ratePerSec,
		burst:      burst,
		nowFunc:    time.Now,
	}
}

// Allow checks whether one unit is allowed for key. If not, it returns false
// and a retryAfter duration.
func (m *Map) Allow(key string) (bool, time.Duration) {
	return m.AllowN(key, 1)
}

// AllowN checks whether n units are allowed for key.
func (m *Map) AllowN(key string, n int) (bool, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFunc()
	var b *Bucket
	if v, ok := m.c.GetMut(key, nil); ok {
		b = v
	} else {
		b = NewBucket(m.ratePerSec, m.burst, now)
		m.c.Put(key, b, nil)
	}
	return b.Take(float64(n), now)
}

// SetWithClock installs a custom clock for tests.
func (m *Map) SetWithClock(now func() time.Time) { m.nowFunc = now }

// CounterMap is a bounded LRU of integer counters, used for per-IP concurrent
// room limits. Increment and Decrement return the resulting count.
type CounterMap struct {
	mu      sync.Mutex
	c       *lru.Cache[int]
	max     int
	onEvict func(key string, val int)
}

// NewCounterMap creates a CounterMap with the given capacity and per-key max.
// onEvict, if non-nil, is invoked when an entry is evicted; this lets the
// caller decrement a related global counter when a per-IP counter disappears.
func NewCounterMap(capacity, max int, onEvict func(key string, val int)) *CounterMap {
	return &CounterMap{
		c:       lru.New[int](capacity),
		max:     max,
		onEvict: onEvict,
	}
}

// Increment increments the counter for key. It returns the new value and
// whether it is within the per-key max. If incrementing would exceed max, the
// counter is left unchanged and (false, current) is returned.
func (m *CounterMap) Increment(key string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, _ := m.c.GetMut(key, nil)
	if cur >= m.max {
		return cur, false
	}
	cur++
	m.c.Put(key, cur, m.onEvict)
	return cur, true
}

// Decrement decrements the counter for key, removing it when it reaches zero.
// It returns the new value.
func (m *CounterMap) Decrement(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.c.GetMut(key, nil)
	if !ok || cur <= 0 {
		return 0
	}
	cur--
	if cur <= 0 {
		m.c.Delete(key)
		return 0
	}
	m.c.Put(key, cur, m.onEvict)
	return cur
}

// Get returns the current counter value for key.
func (m *CounterMap) Get(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, _ := m.c.Get(key)
	return v
}
