// Package server implements the WebSocket signaling endpoint, HTTP operational
// endpoints (/healthz, /metrics), origin checking, the handshake timeout, and
// the WebSocket transport (wsconn.go, poller_linux.go).
//
// It bridges the raw WebSocket transport to the room.Registry by implementing
// room.Conn for each connection.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/gobwas/ws"
	"github.com/kku1993/simple-webrtc-server/internal/config"
	"github.com/kku1993/simple-webrtc-server/internal/metrics"
	"github.com/kku1993/simple-webrtc-server/internal/protocol"
	"github.com/kku1993/simple-webrtc-server/internal/requestlog"
	"github.com/kku1993/simple-webrtc-server/internal/room"
	"github.com/kku1993/simple-webrtc-server/internal/turnstile"
	"github.com/kku1993/simple-webrtc-server/internal/version"
)

// Origin authorization is enforced explicitly in handleSignal via
// cfg.OriginAllowed before the upgrade, so no origin check is configured on the
// upgrader itself: a library default would reject legitimate cross-origin
// requests even when ALLOWED_ORIGINS permits them (including "*"). The frame
// size limit likewise depends on cfg and is applied per connection.
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
		"status":    "ok",
		"uptimeSec": int(s.metrics.Uptime().Seconds()),
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

	conn, brw, _, err := ws.HTTPUpgrader{}.Upgrade(r, w)
	if err != nil {
		s.registry.DecConnections()
		if rec, ok := w.(*requestlog.StatusRecorder); ok {
			rec.SetReason("websocket upgrade failed: " + err.Error())
		}
		if conn != nil {
			_ = conn.Close()
		}
		return
	}
	// net/http hands the hijacked connection's bufio pair to us, but this
	// transport reads frames straight off the net.Conn and never uses it. Copy
	// out anything the client pipelined behind the handshake -- normally
	// nothing -- and let the pair go unreferenced when the handler returns.
	var pre []byte
	if n := brw.Reader.Buffered(); n > 0 {
		peek, _ := brw.Reader.Peek(n)
		pre = append([]byte(nil), peek...)
	}

	c := newWSConn(s, conn, pre, ip)
	s.metrics.ConnectionsLive.Inc()

	// Hand the connection to the transport and return, so net/http can drop the
	// hijacked connection. Blocking here would keep that http.conn reachable for
	// the whole life of the socket, and with it the 4 KB bufio.Reader and 4 KB
	// bufio.Writer it owns -- a cost paid by every connection, on a server whose
	// binding constraint is how many idle sockets fit in memory.
	watch(c)
	c.start()
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
		if r := checkProtocolVersion(m.ProtocolVersion, env.RequestID); r != nil {
			return env, *r
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
		if r := checkProtocolVersion(m.ProtocolVersion, env.RequestID); r != nil {
			return env, *r
		}
		return env, s.registry.JoinRoom(sess, m)

	case protocol.TypeRejoinRoom:
		var m protocol.RejoinRoomMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return env, errResp(protocol.ErrMalformedMessage, "malformed rejoin-room: "+err.Error(), env.RequestID)
		}
		if r := checkProtocolVersion(m.ProtocolVersion, env.RequestID); r != nil {
			return env, *r
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

// checkProtocolVersion verifies that the client's protocolVersion major
// matches the server's. Returns nil when the check passes (or is skipped in
// dev mode — when the server binary was built without ldflags, version.Major()
// is -1 and the check is bypassed so `go test` and plain `go build` work).
func checkProtocolVersion(clientVersion, requestID string) *room.Result {
	serverMajor := version.Major()
	if serverMajor < 0 {
		return nil // dev mode — version not stamped
	}
	if clientVersion == "" {
		r := errResp(protocol.ErrUnsupportedProtocolVersion,
			"missing protocolVersion", requestID)
		return &r
	}
	clientMajor := version.MajorFromString(clientVersion)
	if clientMajor < 0 {
		r := errResp(protocol.ErrUnsupportedProtocolVersion,
			fmt.Sprintf("invalid protocolVersion %q", clientVersion), requestID)
		return &r
	}
	if clientMajor != serverMajor {
		r := errResp(protocol.ErrUnsupportedProtocolVersion,
			fmt.Sprintf("protocol version mismatch: client major %d, server major %d",
				clientMajor, serverMajor), requestID)
		return &r
	}
	return nil
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
