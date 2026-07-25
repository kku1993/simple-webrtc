package requestlog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLoggerNilIsNoOp(t *testing.T) {
	var l *Logger
	// Must not panic.
	l.LogHTTP("GET", "/x", 200, 0, 0, "", "", "", "")
	l.LogWS("1.2.3.4", "create-room", "r1", "room", "create-room-response", nil, nil, 10, 0, "")
}

func TestLogHTTPJSON(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(buf)
	l.LogHTTP("GET", "/healthz", 200, 17, 3*time.Millisecond, "10.0.0.1", "curl/8", "https://app", "")

	entries := decodeLines(t, buf)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e["kind"] != "http" {
		t.Errorf("kind = %v", e["kind"])
	}
	if e["method"] != "GET" {
		t.Errorf("method = %v", e["method"])
	}
	if e["path"] != "/healthz" {
		t.Errorf("path = %v", e["path"])
	}
	if e["status"].(float64) != 200 {
		t.Errorf("status = %v", e["status"])
	}
	if e["bytesOut"].(float64) != 17 {
		t.Errorf("bytesOut = %v", e["bytesOut"])
	}
	if e["remoteIP"] != "10.0.0.1" {
		t.Errorf("remoteIP = %v", e["remoteIP"])
	}
	if e["userAgent"] != "curl/8" {
		t.Errorf("userAgent = %v", e["userAgent"])
	}
	if e["origin"] != "https://app" {
		t.Errorf("origin = %v", e["origin"])
	}
	if e["ts"] == nil {
		t.Errorf("ts missing")
	}
}

func TestLogHTTPWithReason(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(buf)
	l.LogHTTP("GET", "/v1/signal", 403, 0, 0, "10.0.0.1", "", "https://evil", "origin not allowed: https://evil")

	entries := decodeLines(t, buf)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e["reason"] != "origin not allowed: https://evil" {
		t.Errorf("reason = %v", e["reason"])
	}
}

func TestLogWSJSON(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(buf)
	errCode := 1101
	closeCode := 4013
	l.LogWS("10.0.0.2", "join-room", "req-7", "room-abc", "error-response", &errCode, &closeCode, 42, 5*time.Millisecond, "room not found")

	entries := decodeLines(t, buf)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e["kind"] != "ws" {
		t.Errorf("kind = %v", e["kind"])
	}
	if e["msgType"] != "join-room" {
		t.Errorf("msgType = %v", e["msgType"])
	}
	if e["requestId"] != "req-7" {
		t.Errorf("requestId = %v", e["requestId"])
	}
	if e["roomId"] != "room-abc" {
		t.Errorf("roomId = %v", e["roomId"])
	}
	if e["responseType"] != "error-response" {
		t.Errorf("responseType = %v", e["responseType"])
	}
	if e["errorCode"].(float64) != 1101 {
		t.Errorf("errorCode = %v", e["errorCode"])
	}
	if e["closeCode"].(float64) != 4013 {
		t.Errorf("closeCode = %v", e["closeCode"])
	}
	if e["reason"] != "room not found" {
		t.Errorf("reason = %v", e["reason"])
	}
	if e["bytesIn"].(float64) != 42 {
		t.Errorf("bytesIn = %v", e["bytesIn"])
	}
}

func TestLogWSOmitsEmptyOptionalFields(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(buf)
	l.LogWS("10.0.0.2", "signal", "", "", "signal-response", nil, nil, 99, 1*time.Microsecond, "")
	raw := buf.String()
	if strings.Contains(raw, "requestId") {
		t.Errorf("expected requestId omitted, got %s", raw)
	}
	if strings.Contains(raw, "errorCode") {
		t.Errorf("expected errorCode omitted, got %s", raw)
	}
	if strings.Contains(raw, "closeCode") {
		t.Errorf("expected closeCode omitted, got %s", raw)
	}
	if strings.Contains(raw, "reason") {
		t.Errorf("expected reason omitted, got %s", raw)
	}
}

func TestMiddlewareLogsStatusAndBytes(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(buf)
	ipFn := func(r *http.Request) string { return "9.9.9.9" }
	h := l.Middleware(ipFn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/signal", nil)
	req.Header.Set("Origin", "https://app")
	req.Header.Set("User-Agent", "test")
	h.ServeHTTP(rec, req)

	entries := decodeLines(t, buf)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e["status"].(float64) != http.StatusTeapot {
		t.Errorf("status = %v", e["status"])
	}
	if e["bytesOut"].(float64) != 5 {
		t.Errorf("bytesOut = %v", e["bytesOut"])
	}
	if e["remoteIP"] != "9.9.9.9" {
		t.Errorf("remoteIP = %v", e["remoteIP"])
	}
	if e["origin"] != "https://app" {
		t.Errorf("origin = %v", e["origin"])
	}
}

func TestMiddlewarePropagatesReason(t *testing.T) {
	buf := &bytes.Buffer{}
	l := New(buf)
	ipFn := func(r *http.Request) string { return "9.9.9.9" }
	h := l.Middleware(ipFn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec, ok := w.(*StatusRecorder); ok {
			rec.SetReason("origin not allowed: https://evil")
		}
		w.WriteHeader(http.StatusForbidden)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/signal", nil)
	h.ServeHTTP(rec, req)

	entries := decodeLines(t, buf)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e["reason"] != "origin not allowed: https://evil" {
		t.Errorf("reason = %v", e["reason"])
	}
	if e["status"].(float64) != http.StatusForbidden {
		t.Errorf("status = %v", e["status"])
	}
}

func TestStatusRecorderSetReasonFirstWins(t *testing.T) {
	var r StatusRecorder
	r.SetReason("first")
	r.SetReason("second")
	if r.Reason() != "first" {
		t.Errorf("Reason = %q, want first non-empty", r.Reason())
	}
	r2 := StatusRecorder{}
	r2.SetReason("")
	if r2.Reason() != "" {
		t.Errorf("empty SetReason should not set reason")
	}
}

func TestMiddlewareNilLoggerPassthrough(t *testing.T) {
	var l *Logger
	called := false
	h := l.Middleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	h.ServeHTTP(rec, req)
	if !called {
		t.Errorf("handler not called")
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
