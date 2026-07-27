package config

import (
	"os"
	"testing"
)

func setEnv(t *testing.T, k, v string) {
	t.Helper()
	t.Setenv(k, v)
}

func validBaseEnv(t *testing.T) {
	t.Helper()
	// 44 bytes of base64 -> 32 bytes decoded; we use raw bytes here so just
	// provide a 32+ byte string.
	setEnv(t, "SERVER_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef")
	setEnv(t, "ALLOWED_ORIGINS", "https://example.com,https://app.example.com")
	setEnv(t, "SHARD_NAME", "t")
}

func TestLoadDefaults(t *testing.T) {
	validBaseEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PeerDeadlineSec != 600 {
		t.Errorf("PeerDeadlineSec = %d, want 600", c.PeerDeadlineSec)
	}
	if c.RoomMaxLifetimeSec != 5400 {
		t.Errorf("RoomMaxLifetimeSec = %d, want 5400", c.RoomMaxLifetimeSec)
	}
	if c.RejoinTokenTtlSec != 43200 {
		t.Errorf("RejoinTokenTtlSec = %d, want 43200", c.RejoinTokenTtlSec)
	}
	if !c.ReleaseSocketsOnPeerConnected {
		t.Errorf("ReleaseSocketsOnPeerConnected default should be true")
	}
	if c.MaxFrameBytes != 65536 {
		t.Errorf("MaxFrameBytes = %d, want 65536", c.MaxFrameBytes)
	}
	if c.MaxRoomsGlobal != 50000 {
		t.Errorf("MaxRoomsGlobal = %d, want 50000", c.MaxRoomsGlobal)
	}
}

func TestLoadMissingSecretFails(t *testing.T) {
	t.Setenv("SERVER_SECRET", "")
	setEnv(t, "ALLOWED_ORIGINS", "https://example.com")
	// Clear SERVER_SECRET entirely.
	os.Unsetenv("SERVER_SECRET")
	if _, err := Load(); err == nil {
		t.Fatalf("expected error for missing SERVER_SECRET")
	}
}

func TestLoadShortSecretFails(t *testing.T) {
	setEnv(t, "SERVER_SECRET", "tooshort")
	setEnv(t, "ALLOWED_ORIGINS", "https://example.com")
	if _, err := Load(); err == nil {
		t.Fatalf("expected error for short SERVER_SECRET")
	}
}

func TestLoadMissingOriginsFails(t *testing.T) {
	setEnv(t, "SERVER_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef")
	os.Unsetenv("ALLOWED_ORIGINS")
	if _, err := Load(); err == nil {
		t.Fatalf("expected error for missing ALLOWED_ORIGINS")
	}
}

func TestLoadMissingShardFails(t *testing.T) {
	validBaseEnv(t)
	os.Unsetenv("SHARD_NAME")
	if _, err := Load(); err == nil {
		t.Fatalf("expected error for missing SHARD_NAME")
	}
}

func TestLoadInvalidShardFails(t *testing.T) {
	validBaseEnv(t)
	for _, shard := range []string{"us", "0", "i", "o", "l", "u", "", "ab"} {
		t.Setenv("SHARD_NAME", shard)
		if _, err := Load(); err == nil {
			t.Errorf("expected error for SHARD_NAME=%q", shard)
		}
	}
}

func TestLoadValidShard(t *testing.T) {
	validBaseEnv(t)
	// Any single alphabetic Crockford base32 char (case-insensitive) is
	// accepted. The raw value is stored; roomid normalizes case at use.
	for _, shard := range []string{"t", "T", "z", "a"} {
		t.Setenv("SHARD_NAME", shard)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load(SHARD_NAME=%q): %v", shard, err)
		}
		if c.ShardName != shard {
			t.Errorf("ShardName = %q, want %q", c.ShardName, shard)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	c := Config{AllowedOrigins: []string{"https://a.example.com", "https://b.example.com"}}
	if !c.OriginAllowed("https://a.example.com") {
		t.Errorf("expected a allowed")
	}
	if c.OriginAllowed("https://evil.example.com") {
		t.Errorf("expected evil not allowed")
	}
	if c.OriginAllowed("") {
		t.Errorf("empty origin should not be allowed")
	}
}

func TestOriginWildcard(t *testing.T) {
	c := Config{AllowedOrigins: []string{"*"}}
	if !c.OriginAllowed("https://anything.example.com") {
		t.Errorf("wildcard should allow all")
	}
	if !c.OriginsCheckDisabled() {
		t.Errorf("OriginsCheckDisabled should be true")
	}
}

func TestParseOrigins(t *testing.T) {
	got := parseOrigins(" a , b ,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestLoadOverride(t *testing.T) {
	validBaseEnv(t)
	setEnv(t, "PEER_DEADLINE_SEC", "120")
	setEnv(t, "MAX_ROOMS_GLOBAL", "10")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PeerDeadlineSec != 120 {
		t.Errorf("PeerDeadlineSec = %d, want 120", c.PeerDeadlineSec)
	}
	if c.MaxRoomsGlobal != 10 {
		t.Errorf("MaxRoomsGlobal = %d, want 10", c.MaxRoomsGlobal)
	}
}
