package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gobwas/ws"
	"github.com/kku1993/simple-webrtc-server/internal/protocol"
	"github.com/kku1993/simple-webrtc-server/internal/room"
)

// frameBufPool holds scratch buffers used to serialize a WebSocket frame
// (header + payload) so each message costs one write syscall instead of two.
// Unlike a per-connection buffer, a pooled one is held only for the duration of
// a write, which matters here because most sockets are idle: after both peers
// report connected a room's sockets sit open for peerConnectedGraceSec without
// sending anything.
var frameBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledFrameBuf caps what goes back into the pool so one large SDP frame
// does not pin an oversized buffer for the process lifetime.
const maxPooledFrameBuf = 64 << 10

// readChunk is how much a poller-driven read pulls off a socket per syscall.
// The buffer comes from a pool and is held only while a connection is being
// served, never while it is idle, so it can be generous: a signaling message
// fits well inside one, which means one read usually drains the socket.
const readChunk = 16 << 10

var readBufPool = sync.Pool{New: func() any { b := make([]byte, readChunk); return &b }}

// fallbackReadChunk is the same buffer for a connection served by a blocking
// read loop. That buffer is pinned for the connection's life -- the goroutine
// sits in Read holding it -- so it is sized to a typical message instead, and
// anything larger costs an extra read.
const fallbackReadChunk = 4 << 10

// maxPendingBytes caps what a connection will hold for a client that has
// stopped reading. It sits above MaxBufferedSignalBytes (256 KB), the largest
// legitimate burst -- a full signal buffer replayed on reattach -- so hitting
// it means the peer is not draining its socket, not that the burst was big.
const maxPendingBytes = 512 << 10

// writeTimeout bounds a blocking drain of a backed-up socket. Only flush uses
// it; the fast path never blocks and never sets a deadline.
const writeTimeout = 10 * time.Second

// closeError reports that the peer sent a close frame.
type closeError struct {
	code int
	text string
}

func (e *closeError) Error() string {
	return fmt.Sprintf("websocket: close %d (%s)", e.code, e.text)
}

var (
	errFrameTooLarge  = errors.New("websocket: frame exceeds max frame bytes")
	errProtocol       = errors.New("websocket: protocol error")
	errSendBufferFull = errors.New("websocket: send buffer full")
	errConnClosed     = errors.New("websocket: connection closed")
)

// wsConn implements room.Conn over a raw socket, with WebSocket framing
// open-coded on top of gobwas/ws.
//
// The design target is idle sockets, which dominate this server: a room's two
// sockets exchange a couple of dozen messages during setup and then sit open
// for the whole peer-connected grace period saying nothing. Anything held
// per-connection is therefore paid continuously and used almost never, so this
// type keeps nothing per connection that it can avoid:
//
//   - No read buffer. Frames are parsed out of a pooled scratch buffer, and
//     only a connection that receives a partial frame allocates one (rbuf).
//   - No writer goroutine and no send channel. Send serializes the frame and
//     writes it with a non-blocking syscall.Write from the caller's goroutine;
//     only when the kernel send buffer is full does the connection allocate a
//     pending buffer and a goroutine to drain it. This preserves the room.Conn
//     contract that Send never blocks -- the registry calls it under a room
//     mutex.
//   - No reader goroutine, on Linux. Readability comes from epoll (see
//     poller_linux.go) and the message is handled on a goroutine that exits
//     when the socket is drained, so an idle socket costs no stack at all.
//     Other platforms fall back to a goroutine per connection.
//   - No ping ticker goroutine. A single time.AfterFunc per connection drives
//     both keepalive pings and the handshake timeout.
type wsConn struct {
	s    *Server
	sess *room.Session

	conn net.Conn
	rc   syscall.RawConn // non-blocking read/write path; nil if conn exposes no fd
	fd   int             // -1 when rc is nil
	// polled is set when readability is delivered by the poller, which also
	// means reads must not block.
	polled bool
	ip     string

	maxFrame          int64
	pingEvery         time.Duration
	handshakeDeadline time.Time

	// rmu guards the read state and admits one reader at a time. epoll is
	// armed one-shot, so contention is not expected; the lock is what
	// publishes the read state between successive event goroutines.
	rmu     sync.Mutex
	rbuf    []byte // bytes of an incomplete frame; nil in steady state
	msg     []byte // payload of a fragmented message in progress; nil normally
	msgOp   ws.OpCode
	fragged bool

	shook    atomic.Bool // first message dispatched: handshake window is over
	lastRead atomic.Int64

	// wmu guards the write state below and serializes frame writes. It is
	// held only across an append or a single non-blocking syscall, never
	// across a blocking write: draining a backed-up socket happens in flush,
	// which owns the socket via the flushing flag instead of the mutex.
	wmu      sync.Mutex
	out      []byte // frames accepted but not yet written; nil in steady state
	flushing bool   // a flush goroutine owns the socket until out drains
	closed   bool

	timer *time.Timer

	closeOnce   sync.Once
	closeMu     sync.Mutex
	closeCode   *int   // last close code passed to Close, for request logging
	closeReason string // last close reason passed to Close, for request logging
}

func newWSConn(s *Server, conn net.Conn, pre []byte, ip string) *wsConn {
	c := &wsConn{
		s:         s,
		conn:      conn,
		fd:        -1,
		ip:        ip,
		maxFrame:  int64(s.cfg.MaxFrameBytes),
		pingEvery: s.cfg.PingInterval(),
	}
	c.handshakeDeadline = time.Now().Add(s.cfg.HandshakeTimeout())
	c.lastRead.Store(time.Now().UnixNano())
	if len(pre) > 0 {
		// Bytes the client pipelined behind the HTTP handshake. Normally none.
		c.rbuf = append(c.rbuf, pre...)
	}
	// A hijacked plaintext connection is a *net.TCPConn and exposes its fd. A
	// TLS connection does not; those fall back to blocking reads and to the
	// flush path for writes, which is correct but costs a goroutine.
	if sc, ok := conn.(syscall.Conn); ok {
		if rc, err := sc.SyscallConn(); err == nil {
			c.rc = rc
			_ = rc.Control(func(fd uintptr) { c.fd = int(fd) })
		}
	}
	c.sess = room.NewSession(c)
	return c
}

// start arms the connection's timer. It runs after the transport has taken the
// connection, so that everything the timer callback can reach -- and every
// path it can close -- is already published to it.
func (c *wsConn) start() {
	c.wmu.Lock()
	if !c.closed {
		c.timer = time.AfterFunc(time.Until(c.handshakeDeadline), c.tick)
	}
	c.wmu.Unlock()
}

func (c *wsConn) IP() string { return c.ip }

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

// --- write path ---

// Send writes a text frame to the socket. It never blocks: see the type's doc
// comment. It returns false if the connection is closed or the client has
// stopped reading, in which case the connection is closed.
func (c *wsConn) Send(data []byte) bool {
	err := c.writeFrame(ws.NewTextFrame(data))
	switch {
	case err == nil:
		return true
	case errors.Is(err, errConnClosed):
		return false
	case errors.Is(err, errSendBufferFull):
		c.Close(int(protocol.ClosePolicyViolation), "send buffer full")
		return false
	default:
		c.Close(int(protocol.CloseProtocolError), "write error: "+err.Error())
		return false
	}
}

// writeFrame serializes one frame through a pooled buffer and hands the bytes
// to writeBytes. Serializing first means a message costs one write syscall
// rather than one per header and payload.
func (c *wsConn) writeFrame(f ws.Frame) error {
	buf := frameBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	err := ws.WriteFrame(buf, f)
	if err == nil {
		err = c.writeBytes(buf.Bytes())
	}
	if buf.Cap() <= maxPooledFrameBuf {
		frameBufPool.Put(buf)
	}
	return err
}

// writeBytes writes one already-serialized frame, without blocking. b is only
// valid for the duration of the call, so anything not written is copied into
// c.out.
func (c *wsConn) writeBytes(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	if c.closed {
		return errConnClosed
	}
	// A drain is in progress, or bytes are already queued behind one:
	// appending is the only way to keep frames in order.
	if c.flushing || len(c.out) > 0 {
		if len(c.out)+len(b) > maxPendingBytes {
			return errSendBufferFull
		}
		c.out = append(c.out, b...)
		return nil
	}

	n, err := c.tryWrite(b)
	if err != nil {
		return err
	}
	if n < len(b) {
		// The socket buffer filled mid-frame. Hand the rest to a goroutine
		// that may block on it, so this one -- possibly holding a room mutex
		// -- does not.
		c.out = append(c.out, b[n:]...)
		c.flushing = true
		go c.flush()
	}
	return nil
}

// tryWrite writes as much of b as the socket accepts without blocking,
// returning the number of bytes written. A short return means EAGAIN: the
// kernel send buffer is full because the client is not reading. It returns
// (0, nil) when the connection has no raw fd, which routes the write through
// flush instead.
func (c *wsConn) tryWrite(b []byte) (int, error) {
	if c.rc == nil {
		return 0, nil
	}
	var (
		n    int
		werr error
	)
	ctlErr := c.rc.Write(func(fd uintptr) bool {
		for n < len(b) {
			nn, err := syscall.Write(int(fd), b[n:])
			if nn > 0 {
				n += nn
				continue
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				break
			}
			werr = err
			break
		}
		// Always true: returning false asks the runtime to park until the fd
		// is writable, which is exactly what this path exists to avoid.
		return true
	})
	if werr != nil {
		return n, werr
	}
	return n, ctlErr
}

// flush drains c.out, blocking as needed. It runs on its own goroutine and
// owns the socket while c.flushing is set, so no lock is held across the
// blocking write.
func (c *wsConn) flush() {
	for {
		c.wmu.Lock()
		if c.closed || len(c.out) == 0 {
			c.flushing = false
			c.wmu.Unlock()
			return
		}
		buf := c.out
		c.out = nil
		c.wmu.Unlock()

		_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_, err := c.conn.Write(buf)
		_ = c.conn.SetWriteDeadline(time.Time{})
		if err != nil {
			c.wmu.Lock()
			c.flushing = false
			c.out = nil
			c.wmu.Unlock()
			c.Close(int(protocol.CloseProtocolError), "write error: "+err.Error())
			return
		}
	}
}

func (c *wsConn) sendError(code protocol.ErrorCode, msg, reqID string) {
	body, err := json.Marshal(protocol.NewError(code, msg, reqID, nil))
	if err == nil {
		c.Send(body)
	}
}

// --- timers: handshake deadline and keepalive pings ---

// tick enforces the handshake deadline before the first message and the
// ping/pong keepalive after it. One timer per connection covers both; a ticker
// goroutine per connection would not be worth 30 seconds of sleeping.
func (c *wsConn) tick() {
	now := time.Now()
	if !c.shook.Load() {
		if now.Before(c.handshakeDeadline) {
			c.reschedule(c.handshakeDeadline.Sub(now))
			return
		}
		c.sendError(protocol.ErrHandshakeTimeout, "handshake timeout", "")
		c.Close(int(protocol.CloseProtocolError), "handshake timeout")
		return
	}
	// Two consecutive missed pongs close the connection, as before.
	if now.UnixNano()-c.lastRead.Load() > int64(3*c.pingEvery) {
		c.Close(int(protocol.CloseProtocolError), "keepalive timeout")
		return
	}
	if err := c.writeFrame(ws.NewPingFrame(nil)); err != nil {
		if !errors.Is(err, errConnClosed) {
			c.Close(int(protocol.CloseProtocolError), "ping failed: "+err.Error())
		}
		return
	}
	c.reschedule(c.pingEvery)
}

func (c *wsConn) reschedule(d time.Duration) {
	c.wmu.Lock()
	if !c.closed && c.timer != nil {
		c.timer.Reset(d)
	}
	c.wmu.Unlock()
}

// --- close ---

// Close shuts down the connection once, and releases its registry state on a
// separate goroutine: Close is reachable from Send, which the registry calls
// while holding a room mutex that the release path needs.
func (c *wsConn) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		cc := code
		c.closeMu.Lock()
		c.closeCode = &cc
		c.closeReason = reason
		c.closeMu.Unlock()

		// Best-effort close frame, written before the connection is marked
		// closed so writeBytes does not reject it.
		_ = c.writeFrame(ws.NewCloseFrame(ws.NewCloseFrameBody(ws.StatusCode(code), reason)))

		c.wmu.Lock()
		c.closed = true
		c.out = nil
		if c.timer != nil {
			c.timer.Stop()
		}
		c.wmu.Unlock()

		unwatch(c)
		_ = c.conn.Close()
		go c.release()
	})
}

// release returns the connection's registry, metric and log state. It runs
// exactly once, on its own goroutine, after the socket is closed.
func (c *wsConn) release() {
	s := c.s
	s.registry.Detach(c.sess)
	s.registry.DecConnections()
	s.metrics.ConnectionsLive.Dec()
	// A final ws log entry capturing the close code and reason, so operators
	// can see why each connection ended -- including closes initiated by the
	// registry (e.g. close-room -> 4013) that bypass the per-message
	// Result.CloseCode path.
	if s.reqLog != nil {
		s.reqLog.LogWS(c.ip, "close", "", "", "", nil, c.CloseCode(), 0, 0, c.CloseReason())
	}
}

func (c *wsConn) isClosed() bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.closed
}

// --- read path ---

// onReadable drains the socket and dispatches every complete message in it,
// using scratch as its read buffer. It returns false if the connection was
// closed and must not be re-armed.
//
// Called from the poller's event goroutine (Linux) or the fallback read loop.
// The buffer belongs to the caller because the two differ in how long it is
// held: an event goroutine borrows one from a pool for the length of a
// message, while a blocking read loop keeps its buffer parked in Read.
func (c *wsConn) onReadable(scratch []byte) bool {
	c.rmu.Lock()
	defer c.rmu.Unlock()

	for {
		n, err := c.readOnce(scratch)
		if n > 0 {
			c.lastRead.Store(time.Now().UnixNano())
			if !c.consume(scratch[:n]) {
				return false
			}
		}
		switch {
		case err == nil && n == len(scratch):
			// The buffer filled, so there is probably more waiting. A short
			// read means the socket is empty and reading again would only buy
			// an EAGAIN: epoll is level-triggered, so anything still queued is
			// reported again by the re-arm.
			continue
		case err == nil:
			return true
		case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EWOULDBLOCK):
			return true
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			c.closeRead(err)
			return false
		}
	}
}

// readOnce pulls the next chunk off the socket. Poller-driven connections read
// the fd directly and never block: a read that would block returns EAGAIN and
// the caller re-arms the poller. Connections without a poller (no raw fd, or a
// platform without epoll) use the ordinary blocking read.
func (c *wsConn) readOnce(p []byte) (int, error) {
	if !c.polled {
		return c.conn.Read(p)
	}
	var (
		n    int
		rerr error
	)
	ctlErr := c.rc.Read(func(fd uintptr) bool {
		n, rerr = syscall.Read(int(fd), p)
		// Always true: returning false parks the goroutine until the fd is
		// readable, which is the poller's job, not this goroutine's.
		return true
	})
	if rerr != nil {
		return n, rerr
	}
	if ctlErr != nil {
		return n, ctlErr
	}
	if n == 0 {
		// A zero-length read on a raw fd means the peer closed its end.
		return 0, io.EOF
	}
	return n, nil
}

// consume parses complete frames out of b -- prefixed by any bytes left over
// from a previous read -- and dispatches the messages they carry. It returns
// false if the connection was closed.
func (c *wsConn) consume(b []byte) bool {
	if len(c.rbuf) > 0 {
		c.rbuf = append(c.rbuf, b...)
		b = c.rbuf
	}
	off := 0
	for {
		h, hlen, ok, err := parseHeader(b[off:], c.maxFrame)
		if err != nil {
			c.closeRead(err)
			return false
		}
		if !ok {
			break
		}
		end := off + hlen + int(h.Length)
		if end > len(b) {
			break // payload not fully arrived
		}
		payload := b[off+hlen : end]
		off = end
		if h.Masked {
			ws.Cipher(payload, h.Mask, 0)
		}
		if !c.handleFrame(h, payload) {
			return false
		}
	}
	// Keep whatever did not parse. In the common case -- whole messages in one
	// read -- this leaves rbuf nil and the connection holding no read buffer.
	if off == len(b) {
		c.rbuf = c.rbuf[:0]
		if cap(c.rbuf) > readChunk {
			c.rbuf = nil
		}
		return true
	}
	if len(c.rbuf) > 0 {
		c.rbuf = append(c.rbuf[:0], b[off:]...)
	} else {
		c.rbuf = append([]byte(nil), b[off:]...)
	}
	return true
}

// handleFrame acts on one complete frame: control frames are answered here,
// data frames are reassembled and dispatched. It returns false if the
// connection was closed.
func (c *wsConn) handleFrame(h ws.Header, payload []byte) bool {
	switch h.OpCode {
	case ws.OpPing:
		if err := c.writeFrame(ws.NewPongFrame(payload)); err != nil {
			c.closeRead(err)
			return false
		}
		return true
	case ws.OpPong:
		return true
	case ws.OpClose:
		code, text := ws.StatusNoStatusRcvd, ""
		if len(payload) >= 2 {
			code = ws.StatusCode(uint16(payload[0])<<8 | uint16(payload[1]))
			text = string(payload[2:])
		}
		c.closeRead(&closeError{code: int(code), text: text})
		return false
	case ws.OpContinuation:
		if !c.fragged {
			c.closeRead(errProtocol)
			return false
		}
		if int64(len(c.msg))+int64(len(payload)) > c.maxFrame {
			c.closeRead(errFrameTooLarge)
			return false
		}
		c.msg = append(c.msg, payload...)
		if !h.Fin {
			return true
		}
		op, msg := c.msgOp, c.msg
		c.fragged, c.msg, c.msgOp = false, nil, 0
		return c.dispatchMessage(op, msg)
	case ws.OpText, ws.OpBinary:
		if c.fragged {
			c.closeRead(errProtocol)
			return false
		}
		if h.Fin {
			return c.dispatchMessage(h.OpCode, payload)
		}
		c.fragged, c.msgOp = true, h.OpCode
		c.msg = append(c.msg[:0], payload...)
		return true
	default:
		c.closeRead(errProtocol)
		return false
	}
}

// dispatchMessage handles one complete data message. It returns false if the
// connection was closed.
func (c *wsConn) dispatchMessage(op ws.OpCode, raw []byte) bool {
	s := c.s
	if op == ws.OpBinary {
		// Binary frames are a protocol error.
		c.sendError(protocol.ErrMalformedMessage, "binary frames not allowed", "")
		c.Close(int(protocol.CloseProtocolError), "binary frame rejected")
		return false
	}

	// raw points into a pooled read buffer that is reused as soon as this
	// returns. The handlers copy what they keep (JSON decoding into strings),
	// but protocol.Envelope.Raw aliases the input, so hand the dispatcher its
	// own copy rather than depend on nothing downstream retaining it.
	msg := make([]byte, len(raw))
	copy(msg, raw)

	msgStart := time.Now()
	env, result := s.dispatch(c.sess, c, msg)
	if result.Response != nil {
		body, mErr := json.Marshal(result.Response)
		if mErr == nil {
			c.Send(body)
		}
		if errResp, ok := result.Response.(protocol.ErrorResponse); ok {
			s.metrics.RecordError(int(errResp.ErrorCode))
		}
	}
	s.logWS(c.ip, len(msg), env, result, time.Since(msgStart))
	if result.CloseCode != nil {
		c.Close(*result.CloseCode, "policy")
		return false
	}
	// The handshake window closes on the first message; the keepalive deadline
	// takes over from here.
	c.shook.Store(true)
	return true
}

// closeRead closes the connection with the reason implied by a read-side
// error.
func (c *wsConn) closeRead(err error) {
	switch {
	case errors.Is(err, errFrameTooLarge):
		c.Close(int(protocol.CloseProtocolError), "frame too large")
	case isCloseError(err):
		// Client-initiated close or clean disconnect.
		c.Close(int(protocol.CloseProtocolError), "client disconnected: "+err.Error())
	default:
		c.Close(int(protocol.CloseProtocolError), "read error: "+err.Error())
	}
}

// parseHeader decodes a WebSocket frame header from the front of b. ok is
// false when b does not yet hold a complete header, in which case the caller
// waits for more bytes.
//
// The header is parsed in place rather than through ws.ReadHeader because the
// bytes are already in memory: reading it via an io.Reader would mean either a
// per-connection buffered reader (the memory this transport exists to avoid)
// or an allocation per frame.
func parseHeader(b []byte, maxFrame int64) (h ws.Header, hlen int, ok bool, err error) {
	if len(b) < 2 {
		return h, 0, false, nil
	}
	h.Fin = b[0]&0x80 != 0
	if b[0]&0x70 != 0 {
		// RSV bits set, but no extension was negotiated.
		return h, 0, false, errProtocol
	}
	h.OpCode = ws.OpCode(b[0] & 0x0f)
	h.Masked = b[1]&0x80 != 0
	length := int64(b[1] & 0x7f)
	hlen = 2

	control := h.OpCode == ws.OpClose || h.OpCode == ws.OpPing || h.OpCode == ws.OpPong
	if control && (length > 125 || !h.Fin) {
		return h, 0, false, errProtocol
	}

	switch length {
	case 126:
		if len(b) < hlen+2 {
			return h, 0, false, nil
		}
		length = int64(uint16(b[2])<<8 | uint16(b[3]))
		hlen += 2
	case 127:
		if len(b) < hlen+8 {
			return h, 0, false, nil
		}
		var v uint64
		for _, x := range b[2:10] {
			v = v<<8 | uint64(x)
		}
		if v > uint64(maxFrame) {
			return h, 0, false, errFrameTooLarge
		}
		length = int64(v)
		hlen += 8
	}
	if length > maxFrame {
		return h, 0, false, errFrameTooLarge
	}
	if !h.Masked {
		// RFC 6455 §5.1: a client must mask every frame it sends.
		return h, 0, false, errProtocol
	}
	if len(b) < hlen+4 {
		return h, 0, false, nil
	}
	copy(h.Mask[:], b[hlen:hlen+4])
	hlen += 4
	h.Length = length
	return h, hlen, true, nil
}

// isCloseError reports whether err is a WebSocket close error (i.e. the peer
// sent a close frame or the connection was cleanly closed).
func isCloseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ce *closeError
	return errors.As(err, &ce)
}
