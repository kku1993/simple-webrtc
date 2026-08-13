//go:build linux

package server

import (
	"log"
	"sync"
	"sync/atomic"
	"syscall"
)

// The epoll transport. A goroutine parked in Read costs an 8 KB stack that is
// never reclaimed while the socket lives, and this server's steady state is
// tens of thousands of sockets saying nothing. So readability is delivered by
// one epoll instance for the whole process instead, and a message is handled
// on a goroutine that starts when the socket becomes readable and exits when it
// is drained. Idle sockets hold no stack; the goroutine count tracks how many
// connections are actually talking, not how many are open.
//
// Registration is EPOLLONESHOT, so a connection is never handed to two event
// goroutines at once and its read state needs no cross-goroutine coordination
// beyond the re-arm.

// pollerEvents is how many ready fds one epoll_wait collects.
const pollerEvents = 256

// pollWaitMs bounds epoll_wait so the loop notices shutdown. The wait is a
// blocking syscall, so a wakeup a few times a second costs nothing.
const pollWaitMs = 250

type poller struct {
	epfd int

	mu    sync.RWMutex
	conns map[int]*wsConn

	stopped atomic.Bool
}

var (
	pollerOnce sync.Once
	globalPoll *poller
	pollerErr  error
)

// getPoller lazily creates the process-wide poller and starts its event loop.
func getPoller() (*poller, error) {
	pollerOnce.Do(func() {
		fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
		if err != nil {
			pollerErr = err
			return
		}
		globalPoll = &poller{epfd: fd, conns: make(map[int]*wsConn)}
		go globalPoll.run()
	})
	return globalPoll, pollerErr
}

// watch starts serving a connection. It registers the socket with the poller
// and, if the client pipelined a message behind the handshake, dispatches what
// has already arrived.
//
// A connection whose transport exposes no usable fd (TLS) falls back to a
// goroutine per connection.
func watch(c *wsConn) {
	p, err := getPoller()
	if c.rc == nil || c.fd < 0 || err != nil {
		if err != nil {
			log.Printf("epoll unavailable, falling back to blocking reads: %v", err)
		}
		go readLoop(c)
		return
	}
	c.polled = true
	// Read before the socket is registered: once it is, an event goroutine may
	// be consuming the buffer.
	pending := len(c.rbuf) > 0

	p.mu.Lock()
	p.conns[c.fd] = c
	p.mu.Unlock()

	if err := p.ctl(syscall.EPOLL_CTL_ADD, c.fd); err != nil {
		p.mu.Lock()
		delete(p.conns, c.fd)
		p.mu.Unlock()
		c.polled = false
		go readLoop(c)
		return
	}

	// Bytes may already be buffered from the handshake, and the socket may have
	// gone readable before the ADD. Both are covered by draining once now:
	// EPOLLONESHOT means the ADD above has not yet delivered an event that
	// another goroutine could be running.
	if pending {
		go serve(c)
	}
}

// unwatch deregisters a connection. It must run before the fd is closed, so a
// recycled fd cannot be mistaken for this connection.
func unwatch(c *wsConn) {
	if !c.polled {
		return
	}
	p, err := getPoller()
	if err != nil {
		return
	}
	p.mu.Lock()
	delete(p.conns, c.fd)
	p.mu.Unlock()
	_ = p.ctl(syscall.EPOLL_CTL_DEL, c.fd)
}

func (p *poller) ctl(op int, fd int) error {
	ev := &syscall.EpollEvent{Events: epollFlags, Fd: int32(fd)}
	if op == syscall.EPOLL_CTL_DEL {
		ev = nil
	}
	return syscall.EpollCtl(p.epfd, op, fd, ev)
}

// epollOneShot is EPOLLONESHOT, which the syscall package does not export.
// One-shot delivery is what lets a connection's read state be owned by one
// goroutine at a time without a lock around the whole read.
const epollOneShot = 1 << 30

// EPOLLRDHUP reports the peer's half-close, which is how a client going away
// quietly is noticed.
const epollFlags = uint32(syscall.EPOLLIN | syscall.EPOLLRDHUP | epollOneShot)

// rearm re-enables notifications for a connection after its event goroutine
// has drained the socket.
func (p *poller) rearm(c *wsConn) {
	if err := p.ctl(syscall.EPOLL_CTL_MOD, c.fd); err != nil {
		c.Close(4001, "poller rearm failed: "+err.Error())
	}
}

func (p *poller) run() {
	events := make([]syscall.EpollEvent, pollerEvents)
	for !p.stopped.Load() {
		n, err := syscall.EpollWait(p.epfd, events, pollWaitMs)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			log.Printf("epoll_wait: %v", err)
			return
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			p.mu.RLock()
			c := p.conns[fd]
			p.mu.RUnlock()
			if c == nil {
				// Closed between the wait and here; the DEL has already run.
				continue
			}
			go serve(c)
		}
	}
}

// serve drains one readable connection and re-arms it. It runs on its own
// goroutine, which exits as soon as the socket has nothing left to parse -- so
// its stack is charged to the message, not to the connection.
func serve(c *wsConn) {
	bufp := readBufPool.Get().(*[]byte)
	ok := c.onReadable(*bufp)
	readBufPool.Put(bufp)
	if !ok {
		return // closed; unwatch has already run
	}
	if c.isClosed() {
		return
	}
	p, err := getPoller()
	if err != nil {
		return
	}
	p.rearm(c)
}
