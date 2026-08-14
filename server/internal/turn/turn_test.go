package turn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGeneratePostsToIceServersEndpoint(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]},{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"u","credential":"c"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := New("key-123", "token-abc", time.Hour)
	c.SetEndpoint(srv.URL)
	servers, err := c.Generate(context.Background(), "room-ta0000")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/v1/turn/keys/key-123/credentials/generate-ice-servers") {
		t.Errorf("path = %s, want .../generate-ice-servers", gotPath)
	}
	if gotAuth != "Bearer token-abc" {
		t.Errorf("auth = %q, want Bearer token-abc", gotAuth)
	}
	// customIdentifier is echoed in the body.
	var body struct {
		TTL              int    `json:"ttl"`
		CustomIdentifier string `json:"customIdentifier"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.TTL != 3600 {
		t.Errorf("ttl = %d, want 3600", body.TTL)
	}
	if body.CustomIdentifier != "room-ta0000" {
		t.Errorf("customIdentifier = %q, want room-ta0000", body.CustomIdentifier)
	}

	if len(servers) != 2 {
		t.Fatalf("got %d ice servers, want 2", len(servers))
	}
	if servers[0].Username != "" {
		t.Errorf("STUN entry should have no username, got %q", servers[0].Username)
	}
	if servers[1].Username != "u" || servers[1].Credential != "c" {
		t.Errorf("TURN entry = %+v, want username=u credential=c", servers[1])
	}
}

func TestGenerateOmitsCustomIdentifierWhenEmpty(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]}]}`))
	}))
	t.Cleanup(srv.Close)

	c := New("k", "t", time.Hour)
	c.SetEndpoint(srv.URL)
	if _, err := c.Generate(context.Background(), ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(gotBody, "customIdentifier") {
		t.Errorf("body should omit customIdentifier when empty: %s", gotBody)
	}
}

func TestGenerateErrorsOnNon201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := New("k", "bad-token", time.Hour)
	c.SetEndpoint(srv.URL)
	_, err := c.Generate(context.Background(), "r")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want error mentioning 401, got %v", err)
	}
}

func TestGenerateErrorsOnEmptyIceServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"iceServers":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := New("k", "t", time.Hour)
	c.SetEndpoint(srv.URL)
	if _, err := c.Generate(context.Background(), "r"); err == nil {
		t.Fatal("want error for empty ice servers, got nil")
	}
}

func TestGenerateRequiresKeyAndToken(t *testing.T) {
	c := New("", "", time.Hour)
	if _, err := c.Generate(context.Background(), "r"); err == nil {
		t.Fatal("want error when key/token unset, got nil")
	}
}

func TestDefaultTTLWhenZero(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]}]}`))
	}))
	t.Cleanup(srv.Close)

	c := New("k", "t", 0) // 0 → DefaultTTL (4h)
	c.SetEndpoint(srv.URL)
	if _, err := c.Generate(context.Background(), ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var body struct {
		TTL int `json:"ttl"`
	}
	_ = json.Unmarshal([]byte(gotBody), &body)
	if body.TTL != int(DefaultTTL.Seconds()) {
		t.Errorf("ttl = %d, want %d", body.TTL, int(DefaultTTL.Seconds()))
	}
}
