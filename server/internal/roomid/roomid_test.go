package roomid

import (
	"regexp"
	"strings"
	"testing"
)

// validID matches [shard][nid]: 1 alphabetic base32 shard char + 1 base32
// digit + 3 base-10 digits + 1 base32 digit, all lowercase.
var validID = regexp.MustCompile(`^[abcdefghjkmnpqrstvwxyz][0-9a-z][0-9]{3}[0-9a-z]$`)

// TestShard used throughout the suite — a single alphabetic Crockford base32
// char, lowercase.
const testShard = "t"

func TestIsValidShardName_AcceptsAlphabetic(t *testing.T) {
	for _, ch := range "abcdefghjkmnpqrstvwxyz" {
		if !IsValidShardName(string(ch)) {
			t.Errorf("IsValidShardName(%q) = false, want true", ch)
		}
	}
}

func TestIsValidShardName_AcceptsUppercase(t *testing.T) {
	for _, ch := range "ABCDEFGHJKMNPQRSTVWXYZ" {
		if !IsValidShardName(string(ch)) {
			t.Errorf("IsValidShardName(%q) = false, want true", ch)
		}
	}
}

func TestIsValidShardName_RejectsDigits(t *testing.T) {
	for _, ch := range "0123456789" {
		if IsValidShardName(string(ch)) {
			t.Errorf("IsValidShardName(%q) = true, want false (digits reserved)", ch)
		}
	}
}

func TestIsValidShardName_RejectsExcludedAndNonBase32(t *testing.T) {
	for _, ch := range []string{"i", "l", "o", "u", "-", "ab", ""} {
		if IsValidShardName(ch) {
			t.Errorf("IsValidShardName(%q) = true, want false", ch)
		}
	}
}

func TestParseRoomID_CanonicalLowercase(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ParsedRoomID
	}{
		{"ta0000", ParsedRoomID{Shard: "t", NID: "a0000"}},
		{"zz1234", ParsedRoomID{Shard: "z", NID: "z1234"}},
	} {
		got, ok := ParseRoomID(tc.in)
		if !ok {
			t.Fatalf("ParseRoomID(%q) not ok", tc.in)
		}
		if got != tc.want {
			t.Errorf("ParseRoomID(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseRoomID_CrockfordFuzzy(t *testing.T) {
	// Uppercase shard 'A' decodes to 'a'.
	if got, _ := ParseRoomID("AA0000"); got != (ParsedRoomID{Shard: "a", NID: "a0000"}) {
		t.Errorf("AA0000 = %+v", got)
	}
	// nid first digit: 'O' → '0', 'I' → '1', 'L' → '1'.
	if got, _ := ParseRoomID("tO0000"); got != (ParsedRoomID{Shard: "t", NID: "00000"}) {
		t.Errorf("tO0000 = %+v", got)
	}
	if got, _ := ParseRoomID("tI0000"); got != (ParsedRoomID{Shard: "t", NID: "10000"}) {
		t.Errorf("tI0000 = %+v", got)
	}
	if got, _ := ParseRoomID("tL0000"); got != (ParsedRoomID{Shard: "t", NID: "10000"}) {
		t.Errorf("tL0000 = %+v", got)
	}
}

func TestParseRoomID_RejectsWrongLength(t *testing.T) {
	for _, in := range []string{"ta000", "ta00000", ""} {
		if _, ok := ParseRoomID(in); ok {
			t.Errorf("ParseRoomID(%q) ok, want false", in)
		}
	}
}

func TestParseRoomID_RejectsNonBase32Shard(t *testing.T) {
	for _, in := range []string{"ia0000", "ua0000", "-a0000"} {
		if _, ok := ParseRoomID(in); ok {
			t.Errorf("ParseRoomID(%q) ok, want false", in)
		}
	}
}

func TestParseRoomID_RejectsNonBase10InMiddleThree(t *testing.T) {
	if _, ok := ParseRoomID("tab123"); ok { // 'b' is not a base10 digit
		t.Errorf("ParseRoomID(tab123) ok, want false")
	}
	if _, ok := ParseRoomID("ta00a0"); ok {
		t.Errorf("ParseRoomID(ta00a0) ok, want false")
	}
}

func TestParseRoomID_AcceptsAnyBase32LastNIDDigit(t *testing.T) {
	// The last nid digit is base32, like the first.
	for _, tc := range []struct{ in, wantNID string }{
		{"ta000z", "a000z"},
		{"ta000Z", "a000z"},
		{"ta000b", "a000b"},
		{"ta000O", "a0000"}, // Crockford fuzzy: O -> 0
		{"ta000L", "a0001"}, // Crockford fuzzy: L -> 1
	} {
		got, ok := ParseRoomID(tc.in)
		if !ok {
			t.Fatalf("ParseRoomID(%q) not ok", tc.in)
		}
		if got.NID != tc.wantNID {
			t.Errorf("ParseRoomID(%q).NID = %q, want %q", tc.in, got.NID, tc.wantNID)
		}
	}
}

func TestParseRoomID_RejectsNonBase32LastNIDDigit(t *testing.T) {
	// 'u' is excluded from the Crockford alphabet, '-' is not base32 at all.
	for _, in := range []string{"ta000u", "ta000-"} {
		if _, ok := ParseRoomID(in); ok {
			t.Errorf("ParseRoomID(%q) ok, want false", in)
		}
	}
}

func TestParseRoomID_AcceptsAnyBase32FirstNIDDigit(t *testing.T) {
	if got, _ := ParseRoomID("tz0000"); got != (ParsedRoomID{Shard: "t", NID: "z0000"}) {
		t.Errorf("tz0000 = %+v", got)
	}
	if got, _ := ParseRoomID("tZ0000"); got != (ParsedRoomID{Shard: "t", NID: "z0000"}) {
		t.Errorf("tZ0000 = %+v", got)
	}
}

func TestIsValidRoomID(t *testing.T) {
	if !IsValidRoomID("ta0000", "t") {
		t.Errorf("IsValidRoomID(ta0000, t) = false")
	}
	if !IsValidRoomID("zz9999", "z") {
		t.Errorf("IsValidRoomID(zz9999, z) = false")
	}
	if IsValidRoomID("ta0000", "z") {
		t.Errorf("IsValidRoomID(ta0000, z) = true, want false (foreign shard)")
	}
	if !IsValidRoomID("ta0000", "T") {
		t.Errorf("IsValidRoomID(ta0000, T) = false, want true (Crockford decode)")
	}
	if IsValidRoomID("not-an-id", "t") {
		t.Errorf("IsValidRoomID(not-an-id, t) = true, want false")
	}
	if IsValidRoomID("ta000", "t") {
		t.Errorf("IsValidRoomID(ta000, t) = true, want false (too short)")
	}
}

func TestNormalizeRoomID(t *testing.T) {
	// Canonical lowercase input is unchanged.
	if got, ok := NormalizeRoomID("ta0000"); !ok || got != "ta0000" {
		t.Errorf("NormalizeRoomID(ta0000) = %q, ok=%v, want %q", got, ok, "ta0000")
	}
	// Uppercase is folded to lowercase.
	if got, ok := NormalizeRoomID("TA0000"); !ok || got != "ta0000" {
		t.Errorf("NormalizeRoomID(TA0000) = %q, ok=%v, want %q", got, ok, "ta0000")
	}
	// Crockford fuzzy decoding: O→0, I→1, L→1.
	if got, ok := NormalizeRoomID("tO0000"); !ok || got != "t00000" {
		t.Errorf("NormalizeRoomID(tO0000) = %q, ok=%v, want %q", got, ok, "t00000")
	}
	if got, ok := NormalizeRoomID("tI0000"); !ok || got != "t10000" {
		t.Errorf("NormalizeRoomID(tI0000) = %q, ok=%v, want %q", got, ok, "t10000")
	}
	if got, ok := NormalizeRoomID("tL0000"); !ok || got != "t10000" {
		t.Errorf("NormalizeRoomID(tL0000) = %q, ok=%v, want %q", got, ok, "t10000")
	}
	// Mixed case + fuzzy in one id.
	if got, ok := NormalizeRoomID("TZ9999"); !ok || got != "tz9999" {
		t.Errorf("NormalizeRoomID(TZ9999) = %q, ok=%v, want %q", got, ok, "tz9999")
	}
	// Malformed ids return false.
	for _, in := range []string{"ta000", "ta00000", "ia0000", "tab123", "ta000u", "not-an-id", ""} {
		if got, ok := NormalizeRoomID(in); ok {
			t.Errorf("NormalizeRoomID(%q) = %q, ok=true, want false", in, got)
		}
	}
}

func TestGenerate_Format(t *testing.T) {
	id, err := Generate(testShard, func(string) bool { return false })
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !validID.MatchString(id) {
		t.Fatalf("bad format: %q", id)
	}
	if !strings.HasPrefix(id, testShard) {
		t.Fatalf("expected shard prefix %q, got %q", testShard, id)
	}
	if !IsValidRoomID(id, testShard) {
		t.Fatalf("generated id failed IsValidRoomID: %q", id)
	}
}

func TestGenerate_LowercasesShard(t *testing.T) {
	id, err := Generate("T", func(string) bool { return false })
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(id, "t") {
		t.Fatalf("expected lowercased shard prefix, got %q", id)
	}
}

func TestGenerate_WithinCharset(t *testing.T) {
	// The protocol allows [0-9a-z_-]; our IDs use [0-9a-z] (no separators).
	allowed := regexp.MustCompile(`^[0-9a-z]+$`)
	for i := 0; i < 1000; i++ {
		id, err := Generate(testShard, func(string) bool { return false })
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
	id, err := Generate(testShard, exists)
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
	_, err := Generate(testShard, func(string) bool { return true })
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "unique room id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerate_NilCallback(t *testing.T) {
	if _, err := Generate(testShard, nil); err == nil {
		t.Fatalf("expected error for nil callback")
	}
}

func TestGenerate_InvalidShard(t *testing.T) {
	for _, shard := range []string{"us", "0", "i", "", "ab"} {
		if _, err := Generate(shard, func(string) bool { return false }); err != ErrInvalidShard {
			t.Errorf("Generate(%q) err = %v, want ErrInvalidShard", shard, err)
		}
	}
}

func TestGenerate_Distribution(t *testing.T) {
	// Sanity: over many draws, the nid's two base32 digits (32 each) and
	// the 3 base-10 digits between them give a 1,024,000 space. With 5000
	// draws the birthday paradox expects ~12 collisions (~4988 distinct);
	// allow a comfortable margin so this isn't flaky.
	seen := make(map[string]struct{}, 5000)
	for i := 0; i < 5000; i++ {
		id, err := Generate(testShard, func(string) bool { return false })
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		seen[id] = struct{}{}
	}
	if len(seen) < 4950 {
		t.Fatalf("expected ~4988 distinct IDs, got %d", len(seen))
	}
}
