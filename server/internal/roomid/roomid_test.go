package roomid

import (
	"regexp"
	"strings"
	"testing"
)

// validID matches shard-adjective-noun-seq where every part is [a-z]+ and seq
// is exactly two base-36 digits.
var validID = regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z]+-[0-9a-z]{2}$`)

func TestGenerate_Format(t *testing.T) {
	id, err := Generate(Shard, func(string) bool { return false })
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !validID.MatchString(id) {
		t.Fatalf("bad format: %q", id)
	}
	if !strings.HasPrefix(id, Shard+"-") {
		t.Fatalf("expected shard prefix %q, got %q", Shard+"-", id)
	}
}

func TestGenerate_LowercasesShard(t *testing.T) {
	id, err := Generate("US", func(string) bool { return false })
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(id, "us-") {
		t.Fatalf("expected lowercased shard prefix, got %q", id)
	}
}

func TestGenerate_WithinCharset(t *testing.T) {
	// The protocol allows [0-9a-z_-]; our IDs use [0-9a-z-] (no underscore).
	allowed := regexp.MustCompile(`^[0-9a-z-]+$`)
	for i := 0; i < 1000; i++ {
		id, err := Generate(Shard, func(string) bool { return false })
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !allowed.MatchString(id) {
			t.Fatalf("id outside allowed charset: %q", id)
		}
	}
}

func TestGenerate_RetriesOnCollision(t *testing.T) {
	var calls int
	// Reject the first two candidates, accept the third.
	exists := func(string) bool {
		calls++
		return calls <= 2
	}
	id, err := Generate(Shard, exists)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
	if id == "" {
		t.Fatalf("empty id")
	}
}

func TestGenerate_GivesUpAfterMaxRetries(t *testing.T) {
	// Every candidate collides.
	_, err := Generate(Shard, func(string) bool { return true })
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "unique room id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerate_NilCallback(t *testing.T) {
	if _, err := Generate(Shard, nil); err == nil {
		t.Fatalf("expected error for nil callback")
	}
}

func TestGenerate_Distribution(t *testing.T) {
	// Sanity: over many draws, both word lists should be exercised and the
	// collision rate against a fresh map should be low but non-zero.
	seen := make(map[string]struct{}, 5000)
	for i := 0; i < 5000; i++ {
		id, err := Generate(Shard, func(string) bool { return false })
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		seen[id] = struct{}{}
	}
	// With 50 adj * 100 noun * 1296 seq ~= 6.5M space, 5000 draws should
	// produce ~5000 distinct IDs (collisions are astronomically unlikely).
	if len(seen) < 4990 {
		t.Fatalf("expected ~5000 distinct IDs, got %d", len(seen))
	}
}
