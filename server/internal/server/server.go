// Package server implements the WebSocket signaling endpoint, HTTP operational
// endpoints (/healthz, /metrics), origin checking, the handshake timeout, and
// the per-connection read/write loops.
//
// It bridges the raw WebSocket transport to the room.Registry by implementing
// room.Conn for each connection.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/kku1993/simple-peer-signal-server/internal/config"
	"github.com/kku1993/simple-peer-signal-server/internal/metrics"
	"github.com/kku1993/simple-peer-signal-server/internal/protocol"
	"github.com/kku1993/simple-peer-signal-server/internal/requestlog"
	"github.com/kku1993/simple-peer-signal-server/internal/room"
	"github.com/kku1993/simple-peer-signal-server/internal/turnstile"
	"github.com/gorilla/websocket"
)

// upgrader is configured per-connection because the read limit depends on cfg.
// CheckOrigin is disabled here because origin authorization is enforced
// explicitly in handleSignal via cfg.OriginAllowed before the upgrade; relying
// on gorilla's default would reject legitimate cross-origin requests even when
// ALLOWED_ORIGINS permits them (including "*").
var defaultUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// Server is the HTTP/WebSocket server.
type Server struct {
	cfg       config.Config
	registry  *room.Registry
	metrics   *metrics.Metrics
	turnstile *turnstile.Client
	reqLog    *requestlog.Logger

	httpServer *http.Server
}

// New constructs a Server. reqLog may be nil to disable request logging.
func New(cfg config.Config, reg *room.Registry, m *metrics.Metrics, ts *turnstile.Client, reqLog *requestlog.Logger) *Server {
	return &Server{cfg: cfg, registry: reg, metrics: m, turnstile: ts, reqLog: reqLog}
}

// ListenAndServe starts the HTTP server. It blocks until Shutdown is called.
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/metrics", s.metrics.Handler())
	mux.HandleFunc("/v1/signal", s.handleSignal)

	handler := s.reqLog.Middleware(s.resolveIP, mux)

	s.httpServer = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: handler,
	}

	log.Printf("listening on %s", s.cfg.ListenAddr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server and notifies all room connections.
func (s *Server) Shutdown(ctx context.Context) error {
	s.registry.Shutdown()
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"uptimeSec":  int(s.metrics.Uptime().Seconds()),
	})
}

// handleSignal is the WebSocket upgrade endpoint.
func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	// Origin check, before the upgrade.
	if !s.cfg.OriginAllowed(r.Header.Get("Origin")) {
		origin := r.Header.Get("Origin")
		if rec, ok := w.(*requestlog.StatusRecorder); ok {
			rec.SetReason("origin not allowed: " + origin)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":      protocol.TypeErrorResponse,
			"errorCode": int(protocol.ErrOriginNotAllowed),
			"message":   "origin not allowed",
		})
		s.metrics.RecordError(int(protocol.ErrOriginNotAllowed))
		return
	}

	ip := s.resolveIP(r)

	// Per-IP handshake rate limit.
	if ok, wait := s.registry.AllowHandshake(ip); !ok {
		if rec, ok := w.(*requestlog.StatusRecorder); ok {
			rec.SetReason(fmt.Sprintf("handshake rate limited (retry after %ds)", int(wait.Seconds())+1))
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		w.WriteHeader(http.StatusTooManyRequests)
		s.metrics.RateLimitRejects.Inc()
		return
	}

	// Global connection cap.
	if !s.registry.IncConnections() {
		if rec, ok := w.(*requestlog.StatusRecorder); ok {
			rec.SetReason("server at capacity (max connections reached)")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// IncConnections succeeded; we decrement on close (read loop exit or
	// upgrade failure below).

	upgrader := defaultUpgrader
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.registry.DecConnections()
		if rec, ok := w.(*requestlog.StatusRecorder); ok {
			rec.SetReason("websocket upgrade failed: " + err.Error())
		}
		return
	}
	conn.SetReadLimit(int64(s.cfg.MaxFrameBytes))

	// Apply the read deadline for the handshake (first message).
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.HandshakeTimeout()))

	c := newWSConn(conn, ip, s.cfg.PingInterval())
	s.metrics.ConnectionsLive.Inc()

	sess := room.NewSession(c)
	c.runReadLoop(s, sess)
}

// resolveIP determines the client IP per docs/DESIGN.md §"Client IP resolution".
func (s *Server) resolveIP(r *http.Request) string {
	if s.cfg.CloudflareMode {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return ip
		}
	}
	if s.cfg.TrustedProxyCount > 0 {
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			parts := splitComma(xff)
			if n := len(parts); n >= s.cfg.TrustedProxyCount {
				// Nth from the right.
				return parts[n-s.cfg.TrustedProxyCount]
			}
			// Not enough entries; fall back to the immediate peer.
		}
	}
	return r.RemoteAddr
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, trimSpace(s[start:]))
	// Drop empties.
	res := out[:0]
	for _, p := range out {
		if p != "" {
			res = append(res, p)
		}
	}
	return res
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

// dispatch routes an inbound JSON message to the appropriate handler. The
// parsed Envelope is returned alongside the Result so the caller can log the
// inbound message type / request ID even when the handler returns an error.
func (s *Server) dispatch(sess *room.Session, c *wsConn, raw []byte) (protocol.Envelope, room.Result) {
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Envelope{}, room.Result{Response: protocol.NewError(protocol.ErrMalformedMessage, "malformed JSON: "+err.Error(), "", nil)}
	}
	env.Raw = raw

	switch env.Type {
	case protocol.TypeCreateRoom:
		var m protocol.CreateRoomMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed create-room: "+err.Error(), env.RequestID)
		}
		turnstileOK := true
		if s.cfg.TurnstileSecretKey != "" {
			if m.CloudflareTurnstileToken == "" {
				turnstileOK = false
			} else {
				ok, err := s.turnstile.Verify(context.Background(), m.CloudflareTurnstileToken, sess.Conn().IP())
				if err != nil || !ok {
					turnstileOK = false
				}
			}
		}
		return env, s.registry.CreateRoom(sess, m, turnstileOK)

	case protocol.TypeJoinRoom:
		var m protocol.JoinRoomMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed join-room: "+err.Error(), env.RequestID)
		}
		return env, s.registry.JoinRoom(sess, m)

	case protocol.TypeRejoinRoom:
		var m protocol.RejoinRoomMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed rejoin-room: "+err.Error(), env.RequestID)
		}
		return env, s.registry.RejoinRoom(sess, m)

	case protocol.TypeSignal:
		var m protocol.SignalMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed signal: "+err.Error(), env.RequestID)
		}
		return env, s.registry.Signal(sess, m)

	case protocol.TypePeerConnected:
		var m protocol.PeerConnectedMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed peer-connected: "+err.Error(), env.RequestID)
		}
		return env, s.registry.PeerConnected(sess, m)

	case protocol.TypeCloseRoom:
		var m protocol.CloseRoomMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed close-room: "+err.Error(), env.RequestID)
		}
		return env, s.registry.CloseRoom(sess, m)

	default:
		return env, errResp(protocol.ErrUnknownMessageType, "unknown message type: "+env.Type, env.RequestID)
	}
}

func errResp(code protocol.ErrorCode, msg, reqID string) room.Result {
	return room.Result{Response: protocol.NewError(code, msg, reqID, nil)}
}

// logWS emits a JSON request-log entry for one inbound WebSocket message and
// its reply. The response type, protocol error code, room ID, error message,
// and close code are extracted via reflection so this stays decoupled from the
// concrete protocol response structs. The error message is logged as the
// rejection reason so operators can see why a message was rejected without
// correlating against the wire response.
func (s *Server) logWS(ip string, bytesIn int, env protocol.Envelope, result room.Result, duration time.Duration) {
	if s.reqLog == nil {
		return
	}
	respType, roomID, errorCode, reason := responseInfo(result.Response)
	s.reqLog.LogWS(ip, env.Type, env.RequestID, roomID, respType, errorCode, result.CloseCode, bytesIn, duration, reason)
}

// responseInfo extracts the wire `type`, `roomId`, `errorCode`, and `message`
// fields from a protocol response struct via reflection. All return values
// are zero/nil/"" when resp is nil or the field is absent. The `message`
// field is returned as the rejection reason (populated for ErrorResponse).
func responseInfo(resp any) (responseType, roomID string, errorCode *int, reason string) {
	if resp == nil {
		return "", "", nil, ""
	}
	v := reflect.ValueOf(resp)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return "", "", nil, ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", "", nil, ""
	}
	if f := v.FieldByName("Type"); f.IsValid() && f.Kind() == reflect.String {
		responseType = f.String()
	}
	if f := v.FieldByName("RoomID"); f.IsValid() && f.Kind() == reflect.String {
		roomID = f.String()
	}
	if f := v.FieldByName("ErrorCode"); f.IsValid() && f.Kind() == reflect.Int {
		c := int(f.Int())
		errorCode = &c
	}
	if f := v.FieldByName("Message"); f.IsValid() && f.Kind() == reflect.String {
		reason = f.String()
	}
	return responseType, roomID, errorCode, reason
}

// --- WebSocket Conn implementation ---

// wsConn implements room.Conn over a gorilla/websocket connection.
//
// Writes are funnelled through a dedicated writer goroutine reading from a
// buffered channel, so Send is non-blocking and never deadlocks with the room
// mutex. Reads are driven by runReadLoop. Pings are sent on a ticker.
type wsConn struct {
	conn      *websocket.Conn
	ip        string
	pingEvery time.Duration

	sendCh   chan []byte
	closeCh  chan struct{}
	closeOnce sync.Once

	closeMu       sync.Mutex
	closeCode     *int    // last close code passed to Close, for request logging
	closeReason   string  // last close reason passed to Close, for request logging
	writeErr error // set when the writer goroutine exits
}

func newWSConn(c *websocket.Conn, ip string, pingEvery time.Duration) *wsConn {
	return &wsConn{
		conn:      c,
		ip:        ip,
		pingEvery: pingEvery,
		sendCh:    make(chan []byte, 64),
		closeCh:   make(chan struct{}),
	}
}

// Send pushes a message to the writer goroutine. It returns false if the
// connection is closing or the send buffer is full (slow client).
func (c *wsConn) Send(data []byte) bool {
	select {
	case <-c.closeCh:
		return false
	default:
	}
	select {
	case c.sendCh <- data:
		return true
	case <-c.closeCh:
		return false
	default:
		// Buffer full — slow client. Drop and close.
		c.Close(int(protocol.ClosePolicyViolation), "send buffer full")
		return false
	}
}

// Close shuts down the connection once.
func (c *wsConn) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		cc := code
		c.closeMu.Lock()
		c.closeCode = &cc
		c.closeReason = reason
		c.closeMu.Unlock()
		close(c.closeCh)
		// Best-effort close frame; ignore errors.
		msg := websocket.FormatCloseMessage(code, reason)
		_ = c.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		_ = c.conn.Close()
	})
}

// CloseCode returns the close code passed to the first Close call, if any.
func (c *wsConn) CloseCode() *int {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closeCode
}

// CloseReason returns the close reason passed to the first Close call, if any.
func (c *wsConn) CloseReason() string {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closeReason
}

func (c *wsConn) IP() string { return c.ip }

// runReadLoop drives the read side: the handshake deadline, the ping/pong
// handler, and dispatching inbound messages to the registry.
func (c *wsConn) runReadLoop(s *Server, sess *room.Session) {
	// Writer goroutine.
	go c.writeLoop()

	// Ping/pong: close if two consecutive pongs are missed.
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.pingEvery * 3))
		return nil
	})

	defer func() {
		s.registry.Detach(sess)
		s.registry.DecConnections()
		s.metrics.ConnectionsLive.Dec()
		c.Close(int(protocol.CloseProtocolError), "read loop exit")
		// Emit a final ws log entry capturing the close code and reason so
		// operators can see why each connection ended — including closes
		// initiated by the registry (e.g. close-room → 4013) that bypass
		// the per-message Result.CloseCode path.
		if s.reqLog != nil {
			s.reqLog.LogWS(c.ip, "close", "", "", "", nil, c.CloseCode(), 0, 0, c.CloseReason())
		}
	}()

	handshakeDone := false

	for {
		op, raw, err := c.conn.ReadMessage()
		if err != nil {
			// Handshake timeout surfaces as a read deadline error.
			if !handshakeDone && isTimeout(err) {
				_ = c.sendError(protocol.ErrHandshakeTimeout, "handshake timeout", "")
				c.Close(int(protocol.CloseProtocolError), "handshake timeout")
			} else if isCloseError(err) {
				// Client-initiated close or network error; record the
				// reason for the final close log entry.
				c.Close(int(protocol.CloseProtocolError), "client disconnected: "+err.Error())
			} else {
				c.Close(int(protocol.CloseProtocolError), "read error: "+err.Error())
			}
			return
		}
		if op == websocket.BinaryMessage {
			// Binary frames are a protocol error.
			_ = c.sendError(protocol.ErrMalformedMessage, "binary frames not allowed", "")
			c.Close(int(protocol.CloseProtocolError), "binary frame rejected")
			return
		}
		// Reset the read deadline to the ping-based keepalive once attached.
		// Before the handshake, the deadline stays at the handshake timeout.

		msgStart := time.Now()
		env, result := s.dispatch(sess, c, raw)
		if result.Response != nil {
			body, mErr := json.Marshal(result.Response)
			if mErr == nil {
				c.Send(body)
			}
			if errResp, ok := result.Response.(protocol.ErrorResponse); ok {
				s.metrics.RecordError(int(errResp.ErrorCode))
			}
		}
		s.logWS(c.ip, len(raw), env, result, time.Since(msgStart))
		if result.CloseCode != nil {
			c.Close(*result.CloseCode, "policy")
			return
		}

		if !handshakeDone {
			handshakeDone = true
			// Switch to ping-based keepalive deadline.
			_ = c.conn.SetReadDeadline(time.Now().Add(c.pingEvery * 3))
		}
	}
}

func (c *wsConn) writeLoop() {
	pingTicker := time.NewTicker(c.pingEvery)
	defer pingTicker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case msg := <-c.sendCh:
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.Close(int(protocol.CloseProtocolError), "write error")
				return
			}
		case <-pingTicker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
		}
	}
}

func (c *wsConn) sendError(code protocol.ErrorCode, msg, reqID string) error {
	body, _ := json.Marshal(protocol.NewError(code, msg, reqID, nil))
	c.Send(body)
	return nil
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// isCloseError reports whether err is a gorilla/websocket close error (i.e.
// the peer sent a close frame or the connection was cleanly closed).
func isCloseError(err error) bool {
	if err == nil {
		return false
	}
	var ce *websocket.CloseError
	return errors.As(err, &ce)
}
