// Package requestlog emits one JSON line per HTTP request and per inbound
// WebSocket message. It is intentionally dependency-free: the server layer
// extracts the fields and calls LogHTTP / LogWS.
//
// Output is line-delimited JSON written to an injectable io.Writer (os.Stdout
// in production, io.Discard in tests). A nil Logger is safe to call: every
// method is a no-op when l == nil, so callers do not need to guard.
package requestlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

// Logger writes JSON request log lines.
type Logger struct {
	logger *log.Logger
}

// New constructs a Logger that writes to w. Each entry is a single JSON object
// followed by a newline, suitable for ingestion by journald, Loki, or any
// JSON-lines collector.
func New(w io.Writer) *Logger {
	return &Logger{logger: log.New(w, "", 0)}
}

// HTTPEntry is the JSON shape emitted for HTTP requests (including the
// WebSocket upgrade request, which surfaces with status 101). Reason is
// populated for rejected requests (4xx/5xx) and explains why the server
// refused the request.
type HTTPEntry struct {
	Kind       string    `json:"kind"` // always "http"
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	BytesOut   int64     `json:"bytesOut"`
	DurationMs int64     `json:"durationMs"`
	RemoteIP   string    `json:"remoteIP"`
	UserAgent  string    `json:"userAgent,omitempty"`
	Origin     string    `json:"origin,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	TS         time.Time `json:"ts"`
}

// WSEntry is the JSON shape emitted for each inbound WebSocket message. The
// response* fields describe what the server sent back in reply: the response
// message type, the protocol error code (if the response was an error), the
// room ID echoed back (if any), and the WebSocket close code (if the
// connection was closed as a side effect of handling the message). Reason is
// populated for rejected messages and for the final "close" entry.
type WSEntry struct {
	Kind         string    `json:"kind"` // always "ws"
	RemoteIP     string    `json:"remoteIP"`
	MsgType      string    `json:"msgType"`
	RequestID    string    `json:"requestId,omitempty"`
	RoomID       string    `json:"roomId,omitempty"`
	ResponseType string    `json:"responseType,omitempty"`
	ErrorCode    *int      `json:"errorCode,omitempty"`
	CloseCode    *int      `json:"closeCode,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	BytesIn      int       `json:"bytesIn"`
	DurationMs   int64     `json:"durationMs"`
	TS           time.Time `json:"ts"`
}

// LogHTTP emits an HTTPEntry. No-op when l is nil. reason is included
// verbatim when non-empty (typically for 4xx/5xx responses).
func (l *Logger) LogHTTP(method, path string, status int, bytesOut int64, duration time.Duration, ip, ua, origin, reason string) {
	if l == nil {
		return
	}
	e := HTTPEntry{
		Kind:       "http",
		Method:     method,
		Path:       path,
		Status:     status,
		BytesOut:   bytesOut,
		DurationMs: duration.Milliseconds(),
		RemoteIP:   ip,
		UserAgent:  ua,
		Origin:     origin,
		Reason:     reason,
		TS:         time.Now().UTC(),
	}
	if b, err := json.Marshal(e); err == nil {
		l.logger.Println(string(b))
	}
}

// LogWS emits a WSEntry. No-op when l is nil. reason is included verbatim
// when non-empty (typically the protocol error message or the close reason).
func (l *Logger) LogWS(ip, msgType, requestID, roomID, responseType string, errorCode, closeCode *int, bytesIn int, duration time.Duration, reason string) {
	if l == nil {
		return
	}
	e := WSEntry{
		Kind:         "ws",
		RemoteIP:     ip,
		MsgType:      msgType,
		RequestID:    requestID,
		RoomID:       roomID,
		ResponseType: responseType,
		ErrorCode:    errorCode,
		CloseCode:    closeCode,
		Reason:       reason,
		BytesIn:      bytesIn,
		DurationMs:   duration.Milliseconds(),
		TS:           time.Now().UTC(),
	}
	if b, err := json.Marshal(e); err == nil {
		l.logger.Println(string(b))
	}
}

// StatusRecorder wraps an http.ResponseWriter to capture the status code,
// total bytes written, and an optional rejection reason. It forwards
// Hijacker / Flusher so the gorilla/websocket upgrader (which calls Hijack)
// and other middleware keep working through the wrapper.
//
// Handlers that reject a request should call SetReason with a human-readable
// explanation; the middleware reads it back and includes it in the JSON log
// entry.
type StatusRecorder struct {
	http.ResponseWriter
	status   int
	bytesOut int64
	hijacked bool
	reason   string
}

// SetReason records why the handler is rejecting the request. It is safe to
// call multiple times; the first non-empty call wins so an early rejection
// reason is not overwritten by a later generic one.
func (r *StatusRecorder) SetReason(reason string) {
	if r.reason == "" && reason != "" {
		r.reason = reason
	}
}

// Reason returns the recorded rejection reason, if any.
func (r *StatusRecorder) Reason() string {
	return r.reason
}

func (r *StatusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *StatusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytesOut += int64(n)
	return n, err
}

// Hijack forwards to the underlying ResponseWriter so WebSocket upgrades work.
// The hijacked flag is set so the middleware can report status 101 even though
// gorilla/websocket writes the upgrade response directly to the hijacked
// connection (bypassing WriteHeader).
func (r *StatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("requestlog: underlying ResponseWriter is not a Hijacker")
	}
	r.hijacked = true
	return h.Hijack()
}

// Flush forwards to the underlying ResponseWriter if it implements Flusher.
func (r *StatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware wraps h with JSON request logging. The remote IP is extracted
// using ipFn so the server can apply its CF-Connecting-IP / XFF resolution
// policy consistently for both HTTP and WebSocket requests.
func (l *Logger) Middleware(ipFn func(*http.Request) string, h http.Handler) http.Handler {
	if l == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &StatusRecorder{ResponseWriter: w, status: 0}
		h.ServeHTTP(rec, r)
		status := rec.status
		switch {
		case rec.hijacked:
			// The connection was upgraded (WebSocket). gorilla writes the
			// 101 response directly to the hijacked conn, so WriteHeader
			// was never called on the wrapper.
			status = http.StatusSwitchingProtocols
		case status == 0:
			status = http.StatusOK
		}
		l.LogHTTP(r.Method, r.URL.Path, status, rec.bytesOut, time.Since(start),
			ipFn(r), r.Header.Get("User-Agent"), r.Header.Get("Origin"), rec.Reason())
	})
}
