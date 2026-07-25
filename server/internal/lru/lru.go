// Package lru provides a generic, bounded least-recently-used cache.
//
// It is the shared primitive behind the tombstone store, the per-IP room
// counter, and the per-IP rate-limiter maps. All of those need a bounded map
// keyed by string with eviction of the least-recently-used entry on overflow,
// as required by docs/DESIGN.md §"Rate limits" and §"Tombstones".
package lru

import (
	"container/list"
	"sync"
)

// Cache is a thread-safe LRU holding arbitrary values keyed by string.
type Cache[V any] struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	ll       *list.List
}

type entry[V any] struct {
	key string
	val V
}

// New returns a Cache with the given capacity. capacity must be > 0.
func New[V any](capacity int) *Cache[V] {
	if capacity <= 0 {
		panic("lru.New: capacity must be positive")
	}
	return &Cache[V]{
		capacity: capacity,
		items:    make(map[string]*list.Element, capacity),
		ll:       list.New(),
	}
}

// Get returns the value for key and reports whether it was present. Accessing a
// key marks it most-recently-used. If the optional mutate function is supplied,
// it is invoked under the lock with a pointer to the stored value so callers
// can adjust counters in place.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*entry[V]).val, true
}

// GetMut is like Get but hands the caller a pointer to the stored value so
// counters can be mutated in place under the cache lock.
func (c *Cache[V]) GetMut(key string, mutate func(*V)) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	c.ll.MoveToFront(el)
	if mutate != nil {
		mutate(&el.Value.(*entry[V]).val)
	}
	return el.Value.(*entry[V]).val, true
}

// Put inserts or updates key with val, marking it most-recently-used. If the
// cache is over capacity afterwards, the least-recently-used entry is evicted
// and returned via onEvict (if non-nil).
func (c *Cache[V]) Put(key string, val V, onEvict func(k string, v V)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*entry[V]).val = val
		c.ll.MoveToFront(el)
		return
	}
	e := &entry[V]{key: key, val: val}
	el := c.ll.PushFront(e)
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			oe := oldest.Value.(*entry[V])
			c.ll.Remove(oldest)
			delete(c.items, oe.key)
			if onEvict != nil {
				onEvict(oe.key, oe.val)
			}
		}
	}
}

// Delete removes key from the cache. It reports whether a key was removed.
func (c *Cache[V]) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return false
	}
	c.ll.Remove(el)
	delete(c.items, key)
	return true
}

// Len returns the current number of entries.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Has reports whether key is present without updating recency.
func (c *Cache[V]) Has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[key]
	return ok
}

// Range iterates over all entries from most- to least-recently-used. The
// callback is invoked under the cache lock, so it must not call back into the
// cache. Iteration stops if the callback returns false.
func (c *Cache[V]) Range(fn func(key string, val V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry[V])
		if !fn(e.key, e.val) {
			return
		}
	}
}
