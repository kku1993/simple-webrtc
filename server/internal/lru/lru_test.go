package lru

import (
	"fmt"
	"sync"
	"testing"
)

func TestPutGet(t *testing.T) {
	c := New[int](2)
	c.Put("a", 1, nil)
	c.Put("b", 2, nil)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("Get(a) = %d,%v want 1,true", v, ok)
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Errorf("Get(b) = %d,%v want 2,true", v, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Errorf("Get(missing) should be false")
	}
}

func TestEviction(t *testing.T) {
	c := New[int](2)
	c.Put("a", 1, nil)
	c.Put("b", 2, nil)
	c.Get("a") // make a most-recent
	c.Put("c", 3, nil)
	if c.Has("b") {
		t.Errorf("b should have been evicted")
	}
	if !c.Has("a") {
		t.Errorf("a should still be present")
	}
	if !c.Has("c") {
		t.Errorf("c should be present")
	}
}

func TestEvictCallback(t *testing.T) {
	c := New[int](2)
	var evicted []string
	c.Put("a", 1, nil)
	c.Put("b", 2, nil)
	c.Put("c", 3, func(k string, v int) {
		evicted = append(evicted, fmt.Sprintf("%s=%d", k, v))
	})
	if len(evicted) != 1 || evicted[0] != "a=1" {
		t.Errorf("evicted = %v, want [a=1]", evicted)
	}
}

func TestUpdateExisting(t *testing.T) {
	c := New[int](2)
	c.Put("a", 1, nil)
	c.Put("a", 2, nil)
	if v, _ := c.Get("a"); v != 2 {
		t.Errorf("Get(a) = %d, want 2", v)
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}

func TestDelete(t *testing.T) {
	c := New[int](2)
	c.Put("a", 1, nil)
	if !c.Delete("a") {
		t.Errorf("Delete should report true for existing key")
	}
	if c.Has("a") {
		t.Errorf("a should be gone")
	}
	if c.Delete("a") {
		t.Errorf("Delete should report false for missing key")
	}
}

func TestGetMut(t *testing.T) {
	c := New[int](2)
	c.Put("a", 1, nil)
	if v, ok := c.GetMut("a", func(p *int) { *p++ }); !ok || v != 2 {
		t.Errorf("GetMut = %d,%v want 2,true", v, ok)
	}
	if v, _ := c.Get("a"); v != 2 {
		t.Errorf("Get(a) = %d, want 2 (mutated in place)", v)
	}
}

func TestConcurrent(t *testing.T) {
	c := New[int](100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Put(fmt.Sprintf("k-%d-%d", i, j), i+j, nil)
				_, _ = c.Get(fmt.Sprintf("k-%d-%d", i, j))
			}
		}(i)
	}
	wg.Wait()
	if c.Len() > 100 {
		t.Errorf("Len = %d, exceeds capacity 100", c.Len())
	}
}

func TestRange(t *testing.T) {
	c := New[int](3)
	c.Put("a", 1, nil)
	c.Put("b", 2, nil)
	c.Put("c", 3, nil)
	var got []string
	c.Range(func(k string, v int) bool {
		got = append(got, k)
		return true
	})
	// Front is most-recently-used: c, b, a
	want := []string{"c", "b", "a"}
	if len(got) != len(want) {
		t.Fatalf("Range = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Range = %v, want %v", got, want)
		}
	}
}

func TestPanicOnZeroCapacity(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic for zero capacity")
		}
	}()
	New[int](0)
}
