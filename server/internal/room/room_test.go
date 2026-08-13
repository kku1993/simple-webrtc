package room

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/config"
	"github.com/kku1993/simple-webrtc-server/internal/metrics"
	"github.com/kku1993/simple-webrtc-server/internal/protocol"
	"github.com/kku1993/simple-webrtc-server/internal/tombstone"
	"github.com/kku1993/simple-webrtc-server/internal/token"
)

// fakeConn is a recording room.Conn used to drive the registry in tests.
type fakeConn struct {
	ip     string
	mu     sync.Mutex
	sent   [][]byte
	closed bool
	closeCode int
}

func newFakeConn(ip string) *fakeConn { return &fakeConn{ip: ip} }

func (c *fakeConn) Send(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	c.sent = append(c.sent, cp)
	return true
}

func (c *fakeConn) Close(code int, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeCode = code
}

func (c *fakeConn) IP() string { return c.ip }

// messages decodes all sent JSON frames into a slice of maps for assertions.
func (c *fakeConn) messages() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.sent))
	for _, b := range c.sent {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}

func (c *fakeConn) last() map[string]any {
	ms := c.messages()
	if len(ms) == 0 {
		return nil
	}
	return ms[len(ms)-1]
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// --- helpers ---

func testRegistry(t *testing.T) (*Registry, *token.Signer) {
	t.Helper()
	return testRegistryWith(t, nil)
}

// testRegistryWith constructs a registry, applying mutate to the config before
// any rate-limiter or counter map is built.
func testRegistryWith(t *testing.T, mutate func(*config.Config)) (*Registry, *token.Signer) {
	t.Helper()
	cfg := config.Config{
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
	if mutate != nil {
		mutate(&cfg)
	}
	signer, err := token.NewSigner(cfg.ServerSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())
	r := New(cfg, signer, tomb, m)
	return r, signer
}

func isErr(m map[string]any, code int) bool {
	return m["type"] == protocol.TypeErrorResponse && int(m["errorCode"].(float64)) == code
}

// --- tests ---

func TestCreateRoomThenJoin(t *testing.T) {
	r, _ := testRegistry(t)
	hostConn := newFakeConn("1.1.1.1")
	guestConn := newFakeConn("2.2.2.2")
	hs := NewSession(hostConn)
	gs := NewSession(guestConn)

	res := r.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "host-epoch-1",
	}, true)
	if res.Response == nil {
		t.Fatalf("expected create-room-response")
	}
	body, _ := json.Marshal(res.Response)
	var cr protocol.CreateRoomResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cr.RoomID == "" {
		t.Errorf("empty roomId")
	}
	if cr.Role != protocol.RoleHost {
		t.Errorf("role = %s, want host", cr.Role)
	}
	if cr.RejoinToken == "" {
		t.Errorf("empty token")
	}
	if cr.PeerDeadlineInSeconds <= 0 {
		t.Errorf("bad peer deadline: %d", cr.PeerDeadlineInSeconds)
	}

	// Host should not yet have received guest-joined.
	if len(hostConn.messages()) != 0 {
		t.Errorf("host should have no messages yet, got %d", len(hostConn.messages()))
	}

	// Guest joins.
	res = r.JoinRoom(gs, protocol.JoinRoomMsg{
		Type:       protocol.TypeJoinRoom,
		RoomID:     cr.RoomID,
		GuestEpoch: "guest-epoch-1",
	})
	if res.Response == nil {
		t.Fatalf("expected join-room-response")
	}
	body, _ = json.Marshal(res.Response)
	var jr protocol.JoinRoomResponse
	if err := json.Unmarshal(body, &jr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if jr.Role != protocol.RoleGuest {
		t.Errorf("role = %s, want guest", jr.Role)
	}
	if !jr.HostConnected {
		t.Errorf("host should be connected")
	}
	if jr.HostEpoch == nil || *jr.HostEpoch != "host-epoch-1" {
		t.Errorf("hostEpoch = %v, want host-epoch-1", jr.HostEpoch)
	}
	if jr.RejoinToken == "" {
		t.Errorf("guest token empty")
	}

	// Host should have received guest-joined.
	last := hostConn.last()
	if last["type"] != protocol.TypeGuestJoined {
		t.Errorf("host last msg = %v, want guest-joined", last)
	}
	if last["guestEpoch"] != "guest-epoch-1" {
		t.Errorf("guestEpoch = %v", last["guestEpoch"])
	}
}

func TestJoinRoomNotFound(t *testing.T) {
	r, _ := testRegistry(t)
	gs := NewSession(newFakeConn("2.2.2.2"))
	// Valid format + matching shard, but no such room exists.
	res := r.JoinRoom(gs, protocol.JoinRoomMsg{
		Type:       protocol.TypeJoinRoom,
		RoomID:     "tz9999",
		GuestEpoch: "ge",
	})
	if !isErr(res.last(), int(protocol.ErrRoomNotFound)) {
		t.Errorf("expected ROOM_NOT_FOUND, got %v", res.Response)
	}
}

func TestJoinRoomMalformedID(t *testing.T) {
	r, _ := testRegistry(t)
	gs := NewSession(newFakeConn("2.2.2.2"))
	// Wrong length / charset — rejected before any map lookup.
	res := r.JoinRoom(gs, protocol.JoinRoomMsg{
		Type:       protocol.TypeJoinRoom,
		RoomID:     "doesnotexist",
		GuestEpoch: "ge",
	})
	if !isErr(res.last(), int(protocol.ErrMalformedMessage)) {
		t.Errorf("expected MALFORMED_MESSAGE, got %v", res.Response)
	}
}

func TestJoinRoomWrongShard(t *testing.T) {
	r, _ := testRegistry(t)
	gs := NewSession(newFakeConn("2.2.2.2"))
	// Valid format but shard 'z' does not match this instance's 't'.
	res := r.JoinRoom(gs, protocol.JoinRoomMsg{
		Type:       protocol.TypeJoinRoom,
		RoomID:     "zz0000",
		GuestEpoch: "ge",
	})
	if !isErr(res.last(), int(protocol.ErrMalformedMessage)) {
		t.Errorf("expected MALFORMED_MESSAGE for foreign shard, got %v", res.Response)
	}
}

func TestJoinRoomNormalizesUppercaseAndFuzzy(t *testing.T) {
	// Per docs/ROOM_ID_SPEC.md §"Backend handling", the backend must apply
	// Crockford base32 fuzzy decoding and convert to canonical lowercase
	// before lookups. A guest entering "TA0000" or "tO0000" must resolve to
	// the same room stored under the canonical key "ta0000" / "t00000".
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r) // canonical lowercase, e.g. "ta0000"

	// Uppercase version of the same id.
	upper := strings.ToUpper(roomID)
	gs := NewSession(newFakeConn("2.2.2.2"))
	res := r.JoinRoom(gs, protocol.JoinRoomMsg{
		Type:       protocol.TypeJoinRoom,
		RoomID:     upper,
		GuestEpoch: "g",
	})
	if isErr(mapOf(res.Response), int(protocol.ErrRoomNotFound)) {
		t.Fatalf("uppercase %q did not find room %q: %v", upper, roomID, res.Response)
	}
	if isErr(mapOf(res.Response), int(protocol.ErrMalformedMessage)) {
		t.Fatalf("uppercase %q rejected as malformed: %v", upper, res.Response)
	}

	// Crockford fuzzy equivalent: replace the first nid digit with 'O' (→0)
	// or 'I' (→1) if the original was 0 or 1, and verify it still finds the
	// room. We only test the fuzzy case when the nid's first digit is 0 or 1
	// so the fuzzy mapping is reversible.
	fuzzyID := roomID
	if roomID[1] == '0' {
		fuzzyID = roomID[:1] + "O" + roomID[2:]
	} else if roomID[1] == '1' {
		fuzzyID = roomID[:1] + "I" + roomID[2:]
	}
	if fuzzyID != roomID {
		gs2 := NewSession(newFakeConn("3.3.3.3"))
		// The first guest already took the slot, so this gets ROOM_FULL
		// rather than ROOM_NOT_FOUND — which proves the room was found.
		res2 := r.JoinRoom(gs2, protocol.JoinRoomMsg{
			Type:       protocol.TypeJoinRoom,
			RoomID:     fuzzyID,
			GuestEpoch: "g2",
		})
		if isErr(mapOf(res2.Response), int(protocol.ErrRoomNotFound)) {
			t.Fatalf("fuzzy %q did not find room %q: %v", fuzzyID, roomID, res2.Response)
		}
		if isErr(mapOf(res2.Response), int(protocol.ErrMalformedMessage)) {
			t.Fatalf("fuzzy %q rejected as malformed: %v", fuzzyID, res2.Response)
		}
	}
}

func TestJoinRoomFull(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)

	g1 := NewSession(newFakeConn("2.2.2.2"))
	r.JoinRoom(g1, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g1"})

	g2 := NewSession(newFakeConn("3.3.3.3"))
	res := r.JoinRoom(g2, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g2"})
	if !isErr(mapOf(res.Response), int(protocol.ErrRoomFull)) {
		t.Errorf("expected ROOM_FULL, got %v", res.Response)
	}
}

func TestGuestPassword(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(host, protocol.CreateRoomMsg{
		Type:          protocol.TypeCreateRoom,
		HostEpoch:     "h",
		GuestPassword: "secret",
	}, true)
	roomID := getRoomID(t, r)

	// Wrong password.
	g := NewSession(newFakeConn("2.2.2.2"))
	res = r.JoinRoom(g, protocol.JoinRoomMsg{
		Type:          protocol.TypeJoinRoom,
		RoomID:        roomID,
		GuestEpoch:    "g",
		GuestPassword: "wrong",
	})
	if !isErr(mapOf(res.Response), int(protocol.ErrInvalidGuestPassword)) {
		t.Errorf("expected INVALID_GUEST_PASSWORD, got %v", res.Response)
	}

	// Correct password.
	res = r.JoinRoom(g, protocol.JoinRoomMsg{
		Type:          protocol.TypeJoinRoom,
		RoomID:        roomID,
		GuestEpoch:    "g",
		GuestPassword: "secret",
	})
	if isErr(mapOf(res.Response), int(protocol.ErrInvalidGuestPassword)) {
		t.Errorf("expected success, got %v", res.Response)
	}
}

func TestPasswordLockoutWritesTombstone(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(host, protocol.CreateRoomMsg{
		Type:          protocol.TypeCreateRoom,
		HostEpoch:     "h",
		GuestPassword: "secret",
	}, true)
	body, _ := json.Marshal(res.Response)
	var cr protocol.CreateRoomResponse
	_ = json.Unmarshal(body, &cr)
	roomID := cr.RoomID
	hostTok := cr.RejoinToken

	for i := 0; i < r.cfg.MaxPasswordAttempts; i++ {
		g := NewSession(newFakeConn("9.9.9.9"))
		res := r.JoinRoom(g, protocol.JoinRoomMsg{
			Type:          protocol.TypeJoinRoom,
			RoomID:        roomID,
			GuestEpoch:    "g",
			GuestPassword: "wrong",
		})
		_ = res
	}
	if !r.tomb.Has(roomID) {
		t.Errorf("expected tombstone after lockout")
	}

	// Rejoin should now be ROOM_CLOSED.
	host2 := NewSession(newFakeConn("1.1.1.1"))
	res = r.RejoinRoom(host2, protocol.RejoinRoomMsg{
		Type:        protocol.TypeRejoinRoom,
		RejoinToken: hostTok,
		Epoch:       "h2",
	})
	if !isErr(mapOf(res.Response), int(protocol.ErrRoomClosed)) {
		t.Errorf("expected ROOM_CLOSED after lockout, got %v", res.Response)
	}
}

func TestSignalRelay(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"})

	// Host sends a signal; guest should receive it.
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 1, Data: `{"s":"offer"}`})
	last := fc(guest).last()
	if last["type"] != protocol.TypeSignalResponse {
		t.Errorf("guest last = %v, want signal-response", last)
	}
	if last["fromRole"] != "host" {
		t.Errorf("fromRole = %v, want host", last["fromRole"])
	}
	if last["seq"].(float64) != 1 {
		t.Errorf("seq = %v, want 1", last["seq"])
	}
	if last["data"] != `{"s":"offer"}` {
		t.Errorf("data = %v", last["data"])
	}
}

func TestSignalDuplicateSuppressed(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"})

	before := len(fc(guest).messages())
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 5, Data: "a"})
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 5, Data: "a"}) // duplicate
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 3, Data: "b"}) // older
	after := len(fc(guest).messages())
	if after-before != 1 {
		t.Errorf("guest received %d messages, want 1 (duplicates suppressed)", after-before)
	}
}

func TestSignalBufferedWhenPeerAway(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)

	// No guest yet; host signals should buffer.
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 1, Data: "a"})
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 2, Data: "b"})

	// Guest joins with same epoch as... wait, the buffer is at the guest slot.
	// The guest's epoch is set on join. Since the guest slot was never
	// occupied, joining with any epoch is a "first attach" — the buffer is
	// NOT replayed on join (only on same-epoch rejoin). Verify the buffer is
	// held until a same-epoch rejoin.
	guest := NewSession(newFakeConn("2.2.2.2"))
	res := r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g1"})
	_ = res
	// Guest should NOT receive buffered signals on a fresh join.
	for _, m := range fc(guest).messages() {
		if m["type"] == protocol.TypeSignalResponse {
			t.Errorf("fresh join should not replay buffered signals; got %v", m)
		}
	}

	// Now detach the guest and rejoin with the same epoch: buffer should
	// replay. But the buffer was filled BEFORE the guest first joined, and
	// was it cleared when the guest joined? Per the design, the buffer is
	// replayed on "same-epoch rejoin" — i.e. when a slot reconnects with the
	// same epoch it had before. A fresh first join does not replay. The
	// buffer persists across the first join (it was filled when the slot was
	// empty). Actually: when the guest first joins, the slot becomes
	// occupied; signals arriving after that are forwarded live. The
	// pre-join buffer remains in the slot's buffer slice. On a same-epoch
	// rejoin, it would replay. Let's verify by detaching and rejoining.
	r.Detach(guest)
	// Simulate the guest's WS closing.
	guest2 := NewSession(newFakeConn("2.2.2.2"))
	res = r.RejoinRoom(guest2, protocol.RejoinRoomMsg{
		Type:        protocol.TypeRejoinRoom,
		RejoinToken: guestToken(t, r, roomID),
		Epoch:       "g1", // same epoch
	})
	_ = res
	replayed := 0
	for _, m := range fc(guest2).messages() {
		if m["type"] == protocol.TypeSignalResponse {
			replayed++
		}
	}
	if replayed != 2 {
		t.Errorf("expected 2 replayed signals on same-epoch rejoin, got %d", replayed)
	}
}

func TestRejoinNewEpochSendsPeerReset(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h1"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g1"})

	// Guest detaches and rejoins with a NEW epoch.
	r.Detach(guest)
	guest2 := NewSession(newFakeConn("2.2.2.2"))
	res := r.RejoinRoom(guest2, protocol.RejoinRoomMsg{
		Type:        protocol.TypeRejoinRoom,
		RejoinToken: guestToken(t, r, roomID),
		Epoch:       "g2",
	})
	_ = res
	// Host should have received peer-reset.
	last := fc(host).last()
	if last["type"] != protocol.TypePeerReset {
		t.Errorf("host last = %v, want peer-reset", last)
	}
	if last["role"] != "guest" {
		t.Errorf("peer-reset role = %v, want guest", last["role"])
	}
	if last["epoch"] != "g2" {
		t.Errorf("peer-reset epoch = %v, want g2", last["epoch"])
	}
}

func TestRejoinSameEpochSendsPeerRejoined(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h1"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g1"})

	r.Detach(guest)
	guest2 := NewSession(newFakeConn("2.2.2.2"))
	res := r.RejoinRoom(guest2, protocol.RejoinRoomMsg{
		Type:        protocol.TypeRejoinRoom,
		RejoinToken: guestToken(t, r, roomID),
		Epoch:       "g1", // same
	})
	_ = res
	last := fc(host).last()
	if last["type"] != protocol.TypePeerRejoined {
		t.Errorf("host last = %v, want peer-rejoined", last)
	}
}

func TestRejoinRecreatesLostRoom(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h1"}, true)
	body, _ := json.Marshal(res.Response)
	var cr protocol.CreateRoomResponse
	_ = json.Unmarshal(body, &cr)
	roomID := cr.RoomID
	hostToken := cr.RejoinToken

	// Simulate server losing all state: drop the room from the map directly.
	r.mu.Lock()
	delete(r.rooms, roomID)
	r.roomsGlobal.Add(-1)
	r.roomsPerIP.Decrement("1.1.1.1")
	r.mu.Unlock()

	// Guest-first recreate using a guest token... but we only have the host
	// token. Use the host token to recreate.
	host2 := NewSession(newFakeConn("1.1.1.1"))
	res = r.RejoinRoom(host2, protocol.RejoinRoomMsg{
		Type:        protocol.TypeRejoinRoom,
		RejoinToken: hostToken,
		Epoch:       "h2",
	})
	body, _ = json.Marshal(res.Response)
	var rr protocol.RejoinRoomResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !rr.Recreated {
		t.Errorf("expected recreated=true")
	}
	if rr.PeerConnected {
		t.Errorf("expected peerConnected=false")
	}
	if rr.HostEpoch == nil || *rr.HostEpoch != "h2" {
		t.Errorf("hostEpoch = %v, want h2", rr.HostEpoch)
	}
	if rr.GuestEpoch != nil {
		t.Errorf("guestEpoch = %v, want nil", rr.GuestEpoch)
	}
	if rr.PeerDeadlineAt == nil {
		t.Errorf("expected peer deadline set")
	}
}

func TestRejoinEvictsOldConnection(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h1"}, true)
	roomID := getRoomID(t, r)
	tok := hostToken(t, r, roomID)

	// A second host connection rejoin with the same token should evict the
	// first with 4400.
	host2 := NewSession(newFakeConn("1.1.1.1"))
	r.RejoinRoom(host2, protocol.RejoinRoomMsg{
		Type:        protocol.TypeRejoinRoom,
		RejoinToken: tok,
		Epoch:       "h1",
	})
	if !host.conn.(*fakeConn).isClosed() {
		t.Errorf("expected old host conn closed")
	}
	if host.conn.(*fakeConn).closeCode != int(protocol.CloseReplaced) {
		t.Errorf("close code = %d, want 4400", host.conn.(*fakeConn).closeCode)
	}
}

func TestCloseRoomWritesTombstone(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"})

	r.CloseRoom(host, protocol.CloseRoomMsg{Type: protocol.TypeCloseRoom})

	if !r.tomb.Has(roomID) {
		t.Errorf("expected tombstone after close-room")
	}
	if !host.conn.(*fakeConn).isClosed() {
		t.Errorf("host conn should be closed")
	}
	if !guest.conn.(*fakeConn).isClosed() {
		t.Errorf("guest conn should be closed")
	}
	if host.conn.(*fakeConn).closeCode != int(protocol.CloseRoomClosed) {
		t.Errorf("host close code = %d, want 4013", host.conn.(*fakeConn).closeCode)
	}
	// Guest should have received room-closed.
	found := false
	for _, m := range fc(guest).messages() {
		if m["type"] == protocol.TypeRoomClosed {
			found = true
		}
	}
	if !found {
		t.Errorf("guest should have received room-closed")
	}
}

func TestPeerConnectedRelease(t *testing.T) {
	r, _ := testRegistry(t)
	r.cfg.PeerConnectedGraceSec = 0 // release immediately on sweep
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"})

	r.PeerConnected(host, protocol.PeerConnectedMsg{Type: protocol.TypePeerConnected})
	r.PeerConnected(guest, protocol.PeerConnectedMsg{Type: protocol.TypePeerConnected})

	r.SweepOnce()

	if !host.conn.(*fakeConn).isClosed() {
		t.Errorf("host conn should be released")
	}
	if host.conn.(*fakeConn).closeCode != int(protocol.ClosePeerConnected) {
		t.Errorf("host close code = %d, want 4200", host.conn.(*fakeConn).closeCode)
	}
	if r.RoomsGlobal() != 0 {
		t.Errorf("room should be removed, got %d rooms", r.RoomsGlobal())
	}
}

func TestPeerDeadlineExpiry(t *testing.T) {
	r, _ := testRegistry(t)
	r.cfg.PeerDeadlineSec = 1
	now := time.Now()
	r.SetClock(func() time.Time { return now })

	host := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	body, _ := json.Marshal(res.Response)
	var cr protocol.CreateRoomResponse
	_ = json.Unmarshal(body, &cr)
	roomID := cr.RoomID

	// Advance past the peer deadline.
	r.SetClock(func() time.Time { return now.Add(2 * time.Second) })
	r.SweepOnce()

	if !fc(host).isClosed() {
		t.Errorf("host conn should be closed on peer deadline")
	}
	if fc(host).closeCode != int(protocol.CloseRoomExpired) {
		t.Errorf("close code = %d, want 4014", fc(host).closeCode)
	}
	if r.tomb.Has(roomID) {
		t.Errorf("peer deadline expiry must NOT write a tombstone")
	}
	if r.RoomsGlobal() != 0 {
		t.Errorf("room should be removed after expiry, got %d", r.RoomsGlobal())
	}
}

func TestDetachSendsPeerDisconnected(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	guest := NewSession(newFakeConn("2.2.2.2"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)
	r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"})

	r.Detach(guest)
	last := fc(host).last()
	if last["type"] != protocol.TypePeerDisconnected {
		t.Errorf("host last = %v, want peer-disconnected", last)
	}
}

func TestSignalBufferOverflow(t *testing.T) {
	r, _ := testRegistry(t)
	r.cfg.MaxBufferedSignals = 2
	r.cfg.MaxBufferedSignalBytes = 1024
	host := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)

	// Fill the buffer (guest slot is empty).
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 1, Data: "a"})
	r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 2, Data: "b"})
	res := r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 3, Data: "c"})
	if !isErr(mapOf(res.Response), int(protocol.ErrSignalBufferOverflow)) {
		t.Errorf("expected SIGNAL_BUFFER_OVERFLOW, got %v", res.Response)
	}
}

// A second handshake on an attached connection is answered with
// UNEXPECTED_STATE but must NOT close the connection: the socket is healthy and
// still attached, and closing it would break a working session.
func TestUnexpectedStateOnSecondHandshake(t *testing.T) {
	r, _ := testRegistry(t)
	host := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	roomID := getRoomID(t, r)

	cases := map[string]Result{
		"create-room": r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h2"}, true),
		"join-room":   r.JoinRoom(host, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"}),
		"rejoin-room": r.RejoinRoom(host, protocol.RejoinRoomMsg{Type: protocol.TypeRejoinRoom, RejoinToken: "tok", Epoch: "h3"}),
	}
	for name, res := range cases {
		if !isErr(mapOf(res.Response), int(protocol.ErrUnexpectedState)) {
			t.Errorf("%s: expected UNEXPECTED_STATE, got %v", name, res.Response)
		}
		if res.CloseCode != nil {
			t.Errorf("%s: expected no close code, got %v", name, *res.CloseCode)
		}
	}

	// The rejected handshakes left the session attached and usable.
	if !host.attached || host.role != protocol.RoleHost {
		t.Fatalf("session detached by a rejected handshake: attached=%v role=%v", host.attached, host.role)
	}
	if got := getRoomID(t, r); got != roomID {
		t.Errorf("room id changed: %q -> %q", roomID, got)
	}
	if res := r.Signal(host, protocol.SignalMsg{Type: protocol.TypeSignal, Seq: 1, Data: "a"}); res.CloseCode != nil {
		t.Errorf("signaling broken after rejected handshake: close %v", *res.CloseCode)
	}
}

func TestGlobalRoomsCap(t *testing.T) {
	r, _ := testRegistry(t)
	r.cfg.MaxRoomsGlobal = 1
	h1 := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(h1, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	h2 := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(h2, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	if !isErr(mapOf(res.Response), int(protocol.ErrServerAtCapacity)) {
		t.Errorf("expected SERVER_AT_CAPACITY, got %v", res.Response)
	}
}

func TestPerIPRoomLimit(t *testing.T) {
	r, _ := testRegistryWith(t, func(cfg *config.Config) { cfg.MaxRoomsPerIp = 1 })
	h1 := NewSession(newFakeConn("1.1.1.1"))
	r.CreateRoom(h1, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	h2 := NewSession(newFakeConn("1.1.1.1")) // same IP
	res := r.CreateRoom(h2, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
	if !isErr(mapOf(res.Response), int(protocol.ErrRateLimited)) {
		t.Errorf("expected RATE_LIMITED, got %v", res.Response)
	}
}

// --- helpers used by tests ---

func getRoomID(t *testing.T, r *Registry) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rooms) != 1 {
		t.Fatalf("expected exactly 1 room, got %d", len(r.rooms))
	}
	for id := range r.rooms {
		return id
	}
	return ""
}

func hostToken(t *testing.T, r *Registry, roomID string) string {
	t.Helper()
	r.mu.Lock()
	rm, ok := r.rooms[roomID]
	r.mu.Unlock()
	if !ok {
		t.Fatalf("room %s not found", roomID)
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	tok, err := r.signer.Issue(tokenPayloadFor(r, rm, "host"))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func guestToken(t *testing.T, r *Registry, roomID string) string {
	t.Helper()
	r.mu.Lock()
	rm, ok := r.rooms[roomID]
	r.mu.Unlock()
	if !ok {
		t.Fatalf("room %s not found", roomID)
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	tok, err := r.signer.Issue(tokenPayloadFor(r, rm, "guest"))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func tokenPayloadFor(r *Registry, rm *Room, role string) token.Payload {
	return token.Payload{
		V:                 1,
		RoomID:            rm.roomID,
		Role:              role,
		CreatedAt:         rm.createdAt.Unix(),
		GuestPasswordHash: optionalB64(rm.guestPasswordHash),
		GuestPasswordSalt: optionalB64(rm.guestPasswordSalt),
	}
}

func mapOf(v any) map[string]any {
	if v == nil {
		return nil
	}
	body, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return m
}

// fc returns the *fakeConn backing a Session.
func fc(s *Session) *fakeConn { return s.Conn().(*fakeConn) }

// last is a helper on Result.
func (r Result) last() map[string]any { return mapOf(r.Response) }

// scrapeGauge reads a single gauge out of the real Prometheus exposition
// output. It goes through the /metrics handler rather than the in-process
// value so that a gauge which is registered but never set -- the failure this
// test guards against -- is caught.
func scrapeGauge(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 64)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			return v
		}
	}
	t.Fatalf("%s not found in /metrics output", name)
	return 0
}

func TestRoomsLiveGaugeTracksRoomCount(t *testing.T) {
	r, _ := testRegistry(t)

	if got := scrapeGauge(t, r.metrics, "signal_rooms_live"); got != 0 {
		t.Fatalf("signal_rooms_live = %v before any room, want 0", got)
	}

	hs := NewSession(newFakeConn("1.1.1.1"))
	if res := r.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "host-epoch-1",
	}, true); res.Response == nil {
		t.Fatalf("expected create-room-response")
	}
	if got := scrapeGauge(t, r.metrics, "signal_rooms_live"); got != 1 {
		t.Errorf("signal_rooms_live = %v after create, want 1", got)
	}

	hs2 := NewSession(newFakeConn("3.3.3.3"))
	if res := r.CreateRoom(hs2, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "host-epoch-2",
	}, true); res.Response == nil {
		t.Fatalf("expected create-room-response")
	}
	if got := scrapeGauge(t, r.metrics, "signal_rooms_live"); got != 2 {
		t.Errorf("signal_rooms_live = %v after second create, want 2", got)
	}

	r.CloseRoom(hs, protocol.CloseRoomMsg{Type: protocol.TypeCloseRoom})
	if got := scrapeGauge(t, r.metrics, "signal_rooms_live"); got != 1 {
		t.Errorf("signal_rooms_live = %v after close, want 1", got)
	}
}
