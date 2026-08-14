// Package room implements the in-memory room registry, slot state machine,
// signal buffering, epoch tracking, and lifecycle timers described in
// docs/DESIGN.md.
//
// The package is transport-agnostic: it talks to the network through the Conn
// interface. The WebSocket server (internal/server) supplies Conn
// implementations.
package room

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/config"
	"github.com/kku1993/simple-webrtc-server/internal/metrics"
	"github.com/kku1993/simple-webrtc-server/internal/protocol"
	"github.com/kku1993/simple-webrtc-server/internal/ratelimit"
	"github.com/kku1993/simple-webrtc-server/internal/roomid"
	"github.com/kku1993/simple-webrtc-server/internal/tombstone"
	"github.com/kku1993/simple-webrtc-server/internal/token"
	"github.com/kku1993/simple-webrtc-server/internal/turn"
)

// Conn is the network abstraction a room uses to talk to a client. The server
// layer supplies implementations backed by a WebSocket.
//
// Send delivers a complete JSON-encoded message. It must be non-blocking; if
// the underlying connection is slow or closed, Send returns false and the
// caller treats that as a write failure (the connection will be reaped by its
// own writer goroutine).
//
// Close terminates the underlying connection with the given WebSocket close
// code and human-readable reason.
//
// IP returns the resolved client IP used for rate-limiting and the per-IP room
// counter.
type Conn interface {
	Send(data []byte) bool
	Close(code int, reason string)
	IP() string
}

// MaxRoomIDLen caps inbound roomId values before any map lookup, per the design
// doc's payload limits.
const MaxRoomIDLen = 64

// MaxEpochLen caps inbound epoch values.
const MaxEpochLen = 64

// Session is the per-connection state owned by the room registry. The server
// creates one Session per WebSocket and passes it to each handler.
type Session struct {
	conn Conn

	mu       sync.Mutex
	room     *Room
	role     protocol.Role
	attached bool
}

// NewSession constructs a Session for the given Conn.
func NewSession(c Conn) *Session {
	return &Session{conn: c}
}

// Conn returns the underlying Conn.
func (s *Session) Conn() Conn { return s.conn }

// bufferedSignal is one signal held in a slot's buffer, destined for that slot.
//
// Data is the raw JSON string token from the sender, not the decoded payload.
// Unmarshaling into a json.RawMessage copies the bytes out of the read buffer,
// so holding onto it here does not pin the inbound message.
type bufferedSignal struct {
	Seq        int
	FromRole   protocol.Role
	FromEpoch  string
	Data       json.RawMessage
	ReceivedAt time.Time
}

// Slot is one half of a room. See docs/DESIGN.md §"In-memory data model".
type Slot struct {
	conn              Conn
	epoch             string // empty == never occupied in this room instance
	reportedConnected bool
	lastSeq           int
	buffer            []bufferedSignal
	bufferBytes       int
	overflowReset     bool // marked when buffer overflowed; peer must reset
}

// Room is a pairing of two clients. See docs/DESIGN.md §"In-memory data model".
type Room struct {
	reg *Registry

	mu                  sync.Mutex
	roomID              string
	createdAt           time.Time
	instantiatedAt      time.Time
	expiresAt           time.Time
	peerDeadlineAt      *time.Time
	releaseAt           *time.Time // set when both slots reported connected
	guestPasswordHash   []byte
	guestPasswordSalt   []byte
	passwordAttempts    int
	ownerIP             string
	slots               map[protocol.Role]*Slot

	// turnCache holds the iceServers array minted for this room, reused across
	// handshakes (create/join/rejoin) so a single room only hits the TURN API
	// once per credential lifetime. Protected by mu. turnCacheExpiresAt is the
	// earlier of the credential's TTL and the room's expiry; the cache is
	// considered stale once now passes it.
	turnCache          []protocol.IceServer
	turnCacheExpiresAt time.Time
}

// Registry holds all live rooms and the supporting stores.
type Registry struct {
	cfg          config.Config
	signer       *token.Signer
	tomb         *tombstone.Store
	metrics      *metrics.Metrics
	turn         *turn.Client
	roomsPerIP   *ratelimit.CounterMap
	createLimiter *ratelimit.Map
	handshakeLimiter *ratelimit.Map

	mu    sync.Mutex
	rooms map[string]*Room

	roomsGlobal       atomic.Int64
	connectionsGlobal atomic.Int64

	nowFunc func() time.Time
	stopCh  chan struct{}
}

// New constructs a Registry. The signer, tombstone store, and metrics must be
// non-nil; the rate limiters are constructed from the config. turn may be nil
// to disable server-provided TURN credentials (handshake responses then omit
// the iceServers field).
func New(cfg config.Config, signer *token.Signer, tomb *tombstone.Store, m *metrics.Metrics, turn *turn.Client) *Registry {
	r := &Registry{
		cfg:    cfg,
		signer: signer,
		tomb:   tomb,
		metrics: m,
		turn:   turn,
		rooms:  make(map[string]*Room),
		nowFunc: time.Now,
		stopCh: make(chan struct{}),
	}
	// Per-IP room counter: bounded LRU, max = maxRoomsPerIp.
	r.roomsPerIP = ratelimit.NewCounterMap(100000, cfg.MaxRoomsPerIp, nil)
	// create-room + recreating rejoins: 5/min, burst 10 → rate 5/60, burst 10.
	r.createLimiter = ratelimit.NewMap(100000, 5.0/60.0, 10)
	// WebSocket handshakes: 10/min, burst 20.
	r.handshakeLimiter = ratelimit.NewMap(100000, 10.0/60.0, 20)
	return r
}

// SetClock installs a custom clock, primarily for tests.
func (r *Registry) SetClock(now func() time.Time) { r.nowFunc = now }

// IncConnections increments the global connection counter. Returns false if the
// cap has been reached.
func (r *Registry) IncConnections() bool {
	for {
		cur := r.connectionsGlobal.Load()
		if cur >= int64(r.cfg.MaxConnectionsGlobal) {
			return false
		}
		if r.connectionsGlobal.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// DecConnections decrements the global connection counter.
func (r *Registry) DecConnections() {
	r.connectionsGlobal.Add(-1)
}

// AllowHandshake checks the per-IP handshake rate limit. It returns whether the
// handshake is allowed and, if not, a retryAfter duration.
func (r *Registry) AllowHandshake(ip string) (bool, time.Duration) {
	return r.handshakeLimiter.Allow(ip)
}

// RoomsGlobal returns the current live room count.
func (r *Registry) RoomsGlobal() int64 { return r.roomsGlobal.Load() }

// ConnectionsGlobal returns the current connection count.
func (r *Registry) ConnectionsGlobal() int64 { return r.connectionsGlobal.Load() }

// --- helpers ---

func (r *Registry) now() time.Time { return r.nowFunc() }

// googleStunIceServer is always included in the iceServers array so clients can
// gather host and srflx candidates even when TURN is not configured.
var googleStunIceServer = protocol.IceServer{
	URLs: []string{"stun:stun.l.google.com:19302"},
}

var cloudflareStunIceServer = protocol.IceServer{
	URLs: []string{"stun:stun.cloudflare.com:3478"},
}

// generateIceServers builds the iceServers array for a handshake response.
//
// Google's public STUN server is always included so clients can gather host and
// srflx candidates even when TURN is not configured. When TURN is configured,
// short-lived credentials are minted from the Cloudflare Calls TURN API and
// appended after the Google STUN entry. The credential is tagged with
// `roomId-unixTimestamp` for per-session usage analytics — the timestamp
// disambiguates recycled room ids across sessions.
//
// The Cloudflare client has its own HTTP timeout, so context.Background is
// sufficient; the handshake handlers are not on a request goroutine that
// carries a deadline.
func (r *Registry) generateIceServers(roomID string) ([]protocol.IceServer, error) {
	if r.turn == nil {
		return []protocol.IceServer{googleStunIceServer, cloudflareStunIceServer}, nil
	}
	identifier := roomID + "-" + strconv.FormatInt(r.now().Unix(), 10)
	servers, err := r.turn.Generate(context.Background(), identifier)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.IceServer, 0, len(servers)+2)
	out = append(out, googleStunIceServer)
	out = append(out, cloudflareStunIceServer)
	for _, s := range servers {
		out = append(out, protocol.IceServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out, nil
}

// iceServersForRoom returns the room's cached iceServers when they are still
// valid, otherwise mints a fresh set and caches it on the room. The cache is
// valid until the earlier of the minted credential's TTL and the room's
// expiry, matching the lifetime of the credential a client could actually use.
//
// The caller must hold room.mu.
func (r *Registry) iceServersForRoom(room *Room) ([]protocol.IceServer, error) {
	now := r.now()
	if room.turnCache != nil && now.Before(room.turnCacheExpiresAt) {
		return room.turnCache, nil
	}
	iceServers, err := r.generateIceServers(room.roomID)
	if err != nil {
		return nil, err
	}
	room.turnCache = iceServers
	room.turnCacheExpiresAt = r.turnCacheExpiry(now, room.expiresAt)
	return iceServers, nil
}

// turnCacheExpiry returns the instant a cached credential becomes stale: the
// earlier of now+ttl (the credential's lifetime) and the room's expiry (after
// which the room is gone and the credential is useless).
func (r *Registry) turnCacheExpiry(now, roomExpiresAt time.Time) time.Time {
	ttlExpiry := now.Add(r.turnTTL())
	if roomExpiresAt.Before(ttlExpiry) {
		return roomExpiresAt
	}
	return ttlExpiry
}

// turnTTL returns the lifetime of a minted TURN credential. When TURN is not
// configured the value is irrelevant (the cached array is the static STUN
// list), so DefaultTTL is returned as a harmless placeholder.
func (r *Registry) turnTTL() time.Duration {
	if r.turn == nil {
		return turn.DefaultTTL
	}
	return r.turn.TTL()
}

// generateRoomID produces a collision-checked room id of the form
// `[shard][nid]` (see docs/ROOM_ID_SPEC.md and internal/roomid). Collisions
// with live rooms and live tombstones are retried up to roomid.MaxRetries
// times.
func (r *Registry) generateRoomID() (string, error) {
	exists := func(id string) bool {
		r.mu.Lock()
		_, inRooms := r.rooms[id]
		r.mu.Unlock()
		return inRooms || r.tomb.Has(id)
	}
	return roomid.Generate(r.cfg.ShardName, exists)
}

// hashPassword computes sha256(salt || password) and returns the base64-encoded
// hash and the base64-encoded salt.
func hashPassword(salt []byte, password string) (string, string) {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(password))
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), base64.StdEncoding.EncodeToString(salt)
}

// verifyPassword compares the supplied password against the stored hash in
// constant time.
func verifyPassword(salt []byte, storedHash []byte, password string) bool {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(password))
	got := h.Sum(nil)
	return subtle.ConstantTimeCompare(got, storedHash) == 1
}

// genSalt returns 16 random bytes for the password salt.
func genSalt() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// send marshals msg and sends it to conn, returning whether the send succeeded.
func send(conn Conn, msg any) bool {
	if conn == nil {
		return false
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	return conn.Send(body)
}

// sendSignal encodes a signal-response directly into a fresh buffer and sends
// it. It exists so the relay path skips json.Marshal entirely: the payload is
// spliced in as it arrived, and the small fixed fields around it are cheaper
// to write by hand than to reach through reflection for.
func sendSignal(conn Conn, fromRole protocol.Role, fromEpoch string, seq int, data json.RawMessage, receivedAt time.Time) bool {
	if conn == nil {
		return false
	}
	buf := make([]byte, 0, len(data)+128)
	return conn.Send(protocol.AppendSignalResponse(buf, fromRole, fromEpoch, seq, data, isoTime(receivedAt)))
}

// isoTime formats t as an ISO 8601 UTC string.
func isoTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// secondsUntil returns the whole seconds between now and t, floored at 0.
func secondsUntil(now, t time.Time) int {
	d := t.Sub(now)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// ptrStr returns a pointer to s, or nil if s is the empty string. Used for the
// `*string` epoch fields that are nil when a slot has never been occupied.
func ptrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
