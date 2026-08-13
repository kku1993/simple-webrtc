package server

// readLoop serves a connection with a dedicated goroutine blocked in Read. It
// is the fallback for connections the poller cannot take: TLS connections,
// which expose no fd, and platforms without epoll.
//
// Close unblocks the read by closing the socket, so this returns without any
// separate signal.
func readLoop(c *wsConn) {
	buf := make([]byte, fallbackReadChunk)
	for c.onReadable(buf) {
		if c.isClosed() {
			return
		}
	}
}
