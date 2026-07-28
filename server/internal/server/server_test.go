package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/config"
	"github.com/kku1993/simple-webrtc-server/internal/metrics"
	"github.com/kku1993/simple-webrtc-server/internal/requestlog"
	"github.com/kku1993/simple-webrtc-server/internal/room"
	"github.com/kku1993/simple-webrtc-server/internal/tombstone"
	"github.com/kku1993/simple-webrtc-server/internal/token"
	"github.com/gorilla/websocket"
)

// lockedBuffer is a concurrency-safe bytes.Buffer suitable for capturing log
// output from multiple goroutines (the HTTP middleware + WS read loops run on
// separate goroutines).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testConfig() config.Config {
	return config.Config{
		ListenAddr:                    ":0",
		ServerSecret:                  []byte("0123456789abcdef0123456789abcdef0123456789abcdef"),
		AllowedOrigins:                []string{"*"},
		ShardName:                     "t",
		PeerDeadlineSec:               600,
		RoomMaxLifetimeSec:            5400,
		RejoinTokenTtlSec:             43200,
		ReleaseSocketsOnPeerConnected: true,
		PeerConnectedGraceSec:         60,
		MaxFrameBytes:                 65536,
		MaxBufferedSignals:            64,
		MaxBufferedSignalBytes:        262144,
		MaxPasswordAttempts:           5,
		HandshakeTimeoutMs:            10000,
		PingIntervalSec:               30,
		TombstoneMaxEntries:           100000,
		TombstoneTtlSec:               3600,
		MaxRoomsGlobal:                50000,
		MaxConnectionsGlobal:          100000,
		MaxRoomsPerIp:                 20,
	}
}

func newTestServer(t *testing.T, cfg config.Config) (*httptest.Server, *room.Registry) {
	t.Helper()
	signer, err := token.NewSigner(cfg.ServerSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())
	reg := room.New(cfg, signer, tomb, m)
	reg.StartSweep()
	t.Cleanup(reg.Stop)
	srv := New(cfg, reg, m, nil, nil)
	hs := httptest.NewServer(http.HandlerFunc(srv.handleSignal))
	t.Cleanup(hs.Close)
	return hs, reg
}

func dial(t *testing.T, hs *httptest.Server) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/signal"
	c, _, err := websocket.DefaultDialer.DialContext(context.Background(), u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func sendMsg(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func recvMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

func TestServerCreateJoinSignalFlow(t *testing.T) {
	hs, _ := newTestServer(t, testConfig())

	host := dial(t, hs)
	defer host.Close()
	guest := dial(t, hs)
	defer guest.Close()

	sendMsg(t, host, map[string]any{"type": "create-room", "hostEpoch": "h1"})
	cr := recvMsg(t, host)
	if cr["type"] != "create-room-response" {
		t.Fatalf("host got %v, want create-room-response", cr["type"])
	}
	roomID := cr["roomId"].(string)
	if roomID == "" {
		t.Fatalf("empty roomId")
	}

	sendMsg(t, guest, map[string]any{
		"type": "join-room", "roomId": roomID, "guestEpoch": "g1",
	})
	jr := recvMsg(t, guest)
	if jr["type"] != "join-room-response" {
		t.Fatalf("guest got %v, want join-room-response", jr["type"])
	}
	if jr["role"] != "guest" {
		t.Errorf("role = %v", jr["role"])
	}
	if jr["hostEpoch"] != "h1" {
		t.Errorf("hostEpoch = %v, want h1", jr["hostEpoch"])
	}

	// Host should receive guest-joined.
	gj := recvMsg(t, host)
	if gj["type"] != "guest-joined" {
		t.Fatalf("host got %v, want guest-joined", gj["type"])
	}

	// Host sends a signal; guest should receive it.
	sendMsg(t, host, map[string]any{"type": "signal", "seq": 1, "data": `{"offer":true}`})
	sr := recvMsg(t, guest)
	if sr["type"] != "signal-response" {
		t.Fatalf("guest got %v, want signal-response", sr["type"])
	}
	if sr["fromRole"] != "host" {
		t.Errorf("fromRole = %v", sr["fromRole"])
	}
	if sr["data"] != `{"offer":true}` {
		t.Errorf("data = %v", sr["data"])
	}
}

func TestServerOriginCheck(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"https://allowed.example.com"}
	hs, _ := newTestServer(t, cfg)

	u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/signal"
	h := http.Header{}
	h.Set("Origin", "https://evil.example.com")
	_, resp, err := websocket.DefaultDialer.DialContext(context.Background(), u, h)
	if err == nil {
		t.Fatalf("expected dial to fail for disallowed origin")
	}
	if resp == nil {
		t.Fatalf("expected response")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestServerHandshakeTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.HandshakeTimeoutMs = 100
	hs, _ := newTestServer(t, cfg)

	c := dial(t, hs)
	defer c.Close()
	// Don't send anything; expect to be closed.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	if err == nil {
		t.Errorf("expected close/error on handshake timeout")
	}
}

func TestServerBinaryFrameRejected(t *testing.T) {
	hs, _ := newTestServer(t, testConfig())
	c := dial(t, hs)
	defer c.Close()
	if err := c.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"create-room","hostEpoch":"h"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			// Expect close.
			return
		}
	}
}

func TestServerHealthz(t *testing.T) {
	cfg := testConfig()
	signer, _ := token.NewSigner(cfg.ServerSecret)
	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())
	reg := room.New(cfg, signer, tomb, m)
	srv := New(cfg, reg, m, nil, nil)
	hs := httptest.NewServer(http.HandlerFunc(srv.handleHealth))
	defer hs.Close()

	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
	if body["uptimeSec"] == nil {
		t.Errorf("uptimeSec missing")
	}
}

func TestServerUnknownMessageType(t *testing.T) {
	hs, _ := newTestServer(t, testConfig())
	c := dial(t, hs)
	defer c.Close()
	sendMsg(t, c, map[string]any{"type": "frobnicate"})
	m := recvMsg(t, c)
	if m["type"] != "error-response" {
		t.Errorf("got %v, want error-response", m["type"])
	}
	if int(m["errorCode"].(float64)) != int(1002) {
		t.Errorf("errorCode = %v, want 1002", m["errorCode"])
	}
}

func TestServerCloseRoomClosesBoth(t *testing.T) {
	hs, _ := newTestServer(t, testConfig())
	host := dial(t, hs)
	defer host.Close()
	guest := dial(t, hs)
	defer guest.Close()

	sendMsg(t, host, map[string]any{"type": "create-room", "hostEpoch": "h"})
	cr := recvMsg(t, host)
	roomID := cr["roomId"].(string)
	sendMsg(t, guest, map[string]any{"type": "join-room", "roomId": roomID, "guestEpoch": "g"})
	_ = recvMsg(t, guest) // join-room-response
	_ = recvMsg(t, host)  // guest-joined

	sendMsg(t, host, map[string]any{"type": "close-room"})

	// Both should close with 4013.
	checkClose(t, host, 4013)
	checkClose(t, guest, 4013)
}

func checkClose(t *testing.T, c *websocket.Conn, wantCode int) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if ce, ok := err.(*websocket.CloseError); ok {
			if ce.Code != wantCode {
				t.Errorf("close code = %d, want %d", ce.Code, wantCode)
			}
			return
		}
		// Non-close error: acceptable as long as it indicates termination.
		return
	}
}

// To keep the recreate server test honest, replace it with a same-process
// rejoin against a live room.
func TestServerRejoinLiveRoom(t *testing.T) {
	hs, _ := newTestServer(t, testConfig())
	host := dial(t, hs)
	defer host.Close()
	sendMsg(t, host, map[string]any{"type": "create-room", "hostEpoch": "h1"})
	cr := recvMsg(t, host)
	roomID := cr["roomId"].(string)
	hostTok := cr["rejoinToken"].(string)

	// A second host connection rejoin with the same token evicts the first.
	host2 := dial(t, hs)
	defer host2.Close()
	sendMsg(t, host2, map[string]any{
		"type": "rejoin-room", "rejoinToken": hostTok, "epoch": "h1",
	})
	rr := recvMsg(t, host2)
	if rr["type"] != "rejoin-room-response" {
		t.Fatalf("got %v, want rejoin-room-response", rr["type"])
	}
	if rr["recreated"] != false {
		t.Errorf("recreated = %v, want false", rr["recreated"])
	}
	if rr["roomId"] != roomID {
		t.Errorf("roomId = %v, want %s", rr["roomId"], roomID)
	}
}

// TestServerRequestLog verifies that the JSON request logger records both the
// HTTP upgrade (status 101) and each inbound WebSocket message with its
// response type, close code, and rejection reason.
func TestServerRequestLog(t *testing.T) {
	cfg := testConfig()
	signer, _ := token.NewSigner(cfg.ServerSecret)
	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())
	reg := room.New(cfg, signer, tomb, m)
	reg.StartSweep()
	t.Cleanup(reg.Stop)

	logBuf := &lockedBuffer{}
	srv := New(cfg, reg, m, nil, requestlog.New(logBuf))
	hs := httptest.NewServer(srv.reqLog.Middleware(srv.resolveIP, http.HandlerFunc(srv.handleSignal)))
	t.Cleanup(hs.Close)

	host := dial(t, hs)
	defer host.Close()
	guest := dial(t, hs)
	defer guest.Close()

	sendMsg(t, host, map[string]any{"type": "create-room", "hostEpoch": "h1"})
	cr := recvMsg(t, host)
	roomID := cr["roomId"].(string)

	sendMsg(t, guest, map[string]any{
		"type": "join-room", "roomId": roomID, "guestEpoch": "g1",
	})
	_ = recvMsg(t, guest) // join-room-response
	_ = recvMsg(t, host)  // guest-joined

	// Send an unknown message type to trigger an error-response with a reason.
	sendMsg(t, host, map[string]any{"type": "frobnicate"})
	_ = recvMsg(t, host) // error-response

	sendMsg(t, host, map[string]any{"type": "close-room"})
	checkClose(t, host, 4013)
	checkClose(t, guest, 4013)

	// Give the writer goroutines a moment to flush, then drain the buffer.
	time.Sleep(20 * time.Millisecond)

	var httpCount, wsCount int
	var sawUpgrade, sawCreateResp, sawCloseRoom, sawClose4013, sawUnknownReason, sawCloseReason bool
	for _, line := range strings.Split(strings.TrimRight(logBuf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		switch e["kind"] {
		case "http":
			httpCount++
			if e["path"] == "/v1/signal" && e["status"].(float64) == 101 {
				sawUpgrade = true
			}
		case "ws":
			wsCount++
			if e["msgType"] == "create-room" && e["responseType"] == "create-room-response" {
				sawCreateResp = true
				if e["roomId"] != roomID {
					t.Errorf("create-room log roomId = %v, want %s", e["roomId"], roomID)
				}
			}
			if e["msgType"] == "close-room" {
				sawCloseRoom = true
			}
			if e["msgType"] == "frobnicate" && e["reason"] != nil {
				if strings.Contains(e["reason"].(string), "unknown message type") {
					sawUnknownReason = true
				}
			}
			// The registry closes both sockets with 4013 as a side effect
			// of close-room; the read loop emits a final "close" entry.
			if e["msgType"] == "close" && e["closeCode"] != nil && e["closeCode"].(float64) == 4013 {
				sawClose4013 = true
				if e["reason"] != nil && strings.Contains(e["reason"].(string), "room closed") {
					sawCloseReason = true
				}
			}
		}
	}

	if !sawUpgrade {
		t.Errorf("expected an http log entry for the WS upgrade (status 101); httpCount=%d", httpCount)
	}
	if !sawCreateResp {
		t.Errorf("expected a ws log entry for create-room with responseType=create-room-response; wsCount=%d", wsCount)
	}
	if !sawCloseRoom {
		t.Errorf("expected a ws log entry for close-room")
	}
	if !sawClose4013 {
		t.Errorf("expected a ws close entry with closeCode=4013; wsCount=%d", wsCount)
	}
	if !sawCloseReason {
		t.Errorf("expected a ws close entry with reason containing 'room closed'")
	}
	if !sawUnknownReason {
		t.Errorf("expected a ws log entry for unknown message type with a reason")
	}
}

// TestServerRequestLogHTTPRejection verifies that rejected HTTP requests log
// a human-readable reason.
func TestServerRequestLogHTTPRejection(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"https://allowed.example.com"}
	signer, _ := token.NewSigner(cfg.ServerSecret)
	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())
	reg := room.New(cfg, signer, tomb, m)
	reg.StartSweep()
	t.Cleanup(reg.Stop)

	logBuf := &lockedBuffer{}
	srv := New(cfg, reg, m, nil, requestlog.New(logBuf))
	hs := httptest.NewServer(srv.reqLog.Middleware(srv.resolveIP, http.HandlerFunc(srv.handleSignal)))
	t.Cleanup(hs.Close)

	// Disallowed origin → 403 with reason.
	req, _ := http.NewRequest("GET", hs.URL+"/v1/signal", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	time.Sleep(10 * time.Millisecond)

	var found bool
	for _, line := range strings.Split(strings.TrimRight(logBuf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		if e["kind"] == "http" && e["status"].(float64) == 403 {
			reason, _ := e["reason"].(string)
			if strings.Contains(reason, "origin not allowed") && strings.Contains(reason, "https://evil.example.com") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected an http 403 log entry with reason 'origin not allowed: https://evil.example.com'; buf=%s", logBuf.String())
	}
}
