//go:build !linux

package server

// Without epoll there is no way to learn that a socket became readable without
// a goroutine waiting on it, so every connection gets one. This is the
// portable path -- development and tests on non-Linux hosts -- and it costs the
// per-connection stack that poller_linux.go exists to avoid.

func watch(c *wsConn) { go readLoop(c) }

func unwatch(*wsConn) {}
