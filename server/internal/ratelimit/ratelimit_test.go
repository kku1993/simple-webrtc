package ratelimit

import (
	"testing"
	"time"
)

func TestBucketBurst(t *testing.T) {
	now := time.Now()
	b := NewBucket(1, 5, now) // 1/sec, burst 5
	for i := 0; i < 5; i++ {
		if ok, _ := b.Take(1, now); !ok {
			t.Fatalf("take %d should be allowed within burst", i)
		}
	}
	if ok, _ := b.Take(1, now); ok {
		t.Errorf("6th take should be denied")
	}
}

func TestBucketRefill(t *testing.T) {
	now := time.Now()
	b := NewBucket(2, 2, now) // 2/sec, burst 2
	b.Take(2, now)
	if ok, _ := b.Take(1, now); ok {
		t.Errorf("should be empty")
	}
	later := now.Add(time.Second)
	if ok, _ := b.Take(1, later); !ok {
		t.Errorf("should refill 2 tokens in 1s, take 1 allowed")
	}
}

func TestMapAllow(t *testing.T) {
	m := NewMap(100, 1, 2) // 1/sec, burst 2
	if ok, _ := m.Allow("ip1"); !ok {
		t.Errorf("first should be allowed")
	}
	if ok, _ := m.Allow("ip1"); !ok {
		t.Errorf("second should be allowed")
	}
	if ok, _ := m.Allow("ip1"); ok {
		t.Errorf("third should be denied")
	}
	// Different key has its own bucket.
	if ok, _ := m.Allow("ip2"); !ok {
		t.Errorf("ip2 first should be allowed")
	}
}

func TestMapRetryAfter(t *testing.T) {
	now := time.Now()
	m := NewMap(100, 1, 1)
	m.SetWithClock(func() time.Time { return now })
	m.Allow("ip")
	m.SetWithClock(func() time.Time { return now })
	ok, wait := m.Allow("ip")
	if ok {
		t.Errorf("should be denied")
	}
	if wait <= 0 {
		t.Errorf("expected positive retry wait, got %v", wait)
	}
}

func TestCounterMapIncrementDecrement(t *testing.T) {
	cm := NewCounterMap(100, 3, nil)
	if v, ok := cm.Increment("ip"); v != 1 || !ok {
		t.Errorf("Increment 1 = %d,%v want 1,true", v, ok)
	}
	if v, ok := cm.Increment("ip"); v != 2 || !ok {
		t.Errorf("Increment 2 = %d,%v want 2,true", v, ok)
	}
	if v, ok := cm.Increment("ip"); v != 3 || !ok {
		t.Errorf("Increment 3 = %d,%v want 3,true", v, ok)
	}
	if v, ok := cm.Increment("ip"); v != 3 || ok {
		t.Errorf("Increment over max = %d,%v want 3,false", v, ok)
	}
	if v := cm.Decrement("ip"); v != 2 {
		t.Errorf("Decrement = %d want 2", v)
	}
	if v := cm.Get("ip"); v != 2 {
		t.Errorf("Get = %d want 2", v)
	}
}

func TestCounterMapDecrementToZeroRemoves(t *testing.T) {
	cm := NewCounterMap(100, 3, nil)
	cm.Increment("ip")
	cm.Decrement("ip")
	if cm.Get("ip") != 0 {
		t.Errorf("expected 0 after decrement to zero")
	}
	if cm.Get("ip") != 0 {
		t.Errorf("expected still 0")
	}
}

func TestCounterMapEvictCallback(t *testing.T) {
	var evicted []string
	cm := NewCounterMap(2, 10, func(k string, v int) {
		evicted = append(evicted, k)
	})
	cm.Increment("a")
	cm.Increment("b")
	cm.Increment("c") // evicts a (LRU)
	if len(evicted) != 1 || evicted[0] != "a" {
		t.Errorf("evicted = %v, want [a]", evicted)
	}
}
