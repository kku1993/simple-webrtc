package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func mustSecret(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func TestIssueAndVerify(t *testing.T) {
	s, err := NewSigner(mustSecret(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	p := Payload{
		RoomID:    "abc123",
		Role:      "host",
		CreatedAt: time.Now().Unix(),
	}
	tok, err := s.Issue(p)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.RoomID != p.RoomID || got.Role != p.Role {
		t.Errorf("roundtrip mismatch: %+v vs %+v", got, p)
	}
	if got.V != 1 {
		t.Errorf("V = %d, want 1", got.V)
	}
}

func TestVerifyRejectsBadSignature(t *testing.T) {
	s, _ := NewSigner(mustSecret(t))
	tok, _ := s.Issue(Payload{RoomID: "r", Role: "host", CreatedAt: time.Now().Unix()})
	// Flip a character in the signature portion.
	tampered := tok[:len(tok)-1]
	last := tok[len(tok)-1]
	if last == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	if _, err := s.Verify(tampered); err == nil {
		t.Fatalf("expected error on tampered signature")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	s1, _ := NewSigner(mustSecret(t))
	s2, _ := NewSigner(mustSecret(t))
	tok, _ := s1.Issue(Payload{RoomID: "r", Role: "host", CreatedAt: time.Now().Unix()})
	if _, err := s2.Verify(tok); err == nil {
		t.Fatalf("expected error for different secret")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	s, _ := NewSigner(mustSecret(t))
	for _, in := range []string{"", "v1", "v1.aaa", "v1.aaa.bbb.ccc", "x.y.z", "v1.!!!.zzz"} {
		if _, err := s.Verify(in); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestVerifyRejectsBadRole(t *testing.T) {
	s, _ := NewSigner(mustSecret(t))
	// Manually craft a payload with bad role, signed correctly.
	body := []byte(`{"v":1,"roomId":"r","role":"admin","createdAt":1}`)
	payloadB64 := base64.RawURLEncoding.EncodeToString(body)
	signingInput := "v1." + payloadB64
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	tok := signingInput + "." + sig
	if _, err := s.Verify(tok); err == nil {
		t.Fatalf("expected error for bad role")
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	p := Payload{CreatedAt: now.Add(-2 * time.Hour).Unix()}
	if !Expired(p, now, time.Hour) {
		t.Errorf("expected expired")
	}
	if Expired(p, now, 3*time.Hour) {
		t.Errorf("expected not expired")
	}
}

func TestNewSignerRejectsShortSecret(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatalf("expected error for short secret")
	}
}
