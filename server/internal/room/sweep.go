package room

import (
	"crypto/rand"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/protocol"
)

// StartSweep launches the background timer goroutine that drives peer
// deadlines, room expiry, and tombstone TTLs. It runs until Stop is called.
//
// Per the design doc, a single sweep goroutine on a ~5s tick is used instead of
// allocating a timer per room.
func (r *Registry) StartSweep() {
	go r.sweepLoop()
}

// Stop terminates the sweep goroutine. It is safe to call multiple times.
func (r *Registry) Stop() {
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}
}

func (r *Registry) sweepLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.sweepOnce()
		}
	}
}

// SweepOnce performs one sweep pass. Exported for tests.
func (r *Registry) SweepOnce() { r.sweepOnce() }

func (r *Registry) sweepOnce() {
	now := r.now()
	r.tomb.Sweep()

	r.mu.Lock()
	rooms := make([]*Room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		rooms = append(rooms, rm)
	}
	r.mu.Unlock()

	for _, room := range rooms {
		r.sweepRoom(room, now)
	}
}

// sweepRoom checks one room against the three timers and the peer-connected
// release. It may remove the room from the registry.
func (r *Registry) sweepRoom(room *Room, now time.Time) {
	room.mu.Lock()
	defer room.mu.Unlock()

	// Peer-connected grace release.
	if room.releaseAt != nil && !now.Before(*room.releaseAt) {
		r.releaseRoomLocked(room, "peer-connected", protocol.ClosePeerConnected)
		return
	}

	// Room expiry (in-memory lifetime or token TTL).
	if !now.Before(room.expiresAt) {
		r.expireRoomLocked(room)
		return
	}

	// Peer deadline: only when exactly one slot is occupied.
	if room.peerDeadlineAt != nil && !now.Before(*room.peerDeadlineAt) {
		r.expireRoomLocked(room)
		return
	}
}

// expireRoomLocked sends room-expired to occupied slots, closes them with 4014,
// and removes the room. No tombstone is written (rejoin still allowed).
func (r *Registry) expireRoomLocked(room *Room) {
	for _, slot := range room.slots {
		if slot.conn != nil {
			send(slot.conn, protocol.RoomExpiredEvent{Type: protocol.TypeRoomExpired})
			slot.conn.Close(int(protocol.CloseRoomExpired), "room expired")
			slot.conn = nil
		}
	}
	r.removeRoomLocked(room)
	r.metrics.RoomsExpired.Inc()
}

// releaseRoomLocked sends room-idle-close to both slots, closes them with 4200,
// and removes the room. No tombstone.
func (r *Registry) releaseRoomLocked(room *Room, reason string, code protocol.CloseCode) {
	for _, slot := range room.slots {
		if slot.conn != nil {
			send(slot.conn, protocol.RoomIdleCloseEvent{
				Type:   protocol.TypeRoomIdleClose,
				Reason: reason,
			})
			slot.conn.Close(int(code), reason)
			slot.conn = nil
		}
	}
	r.removeRoomLocked(room)
	r.metrics.RoomsReleased.Inc()
}

// Shutdown sends server-shutdown to every attached connection with a random
// reconnectAfterMs in [1000, 15000] and closes it with 4300. Rooms are removed
// from memory but NO tombstones are written (rejoin is still permitted while
// tokens are valid).
func (r *Registry) Shutdown() {
	r.mu.Lock()
	rooms := make([]*Room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		rooms = append(rooms, rm)
	}
	r.mu.Unlock()

	for _, room := range rooms {
		room.mu.Lock()
		for _, slot := range room.slots {
			if slot.conn != nil {
				send(slot.conn, protocol.ServerShutdownEvent{
					Type:             protocol.TypeServerShutdown,
					ReconnectAfterMs: randomReconnectMs(),
				})
				slot.conn.Close(int(protocol.CloseServerShutdown), "server shutting down")
				slot.conn = nil
			}
		}
		room.mu.Unlock()
		r.removeRoomLocked(room)
	}
}

// randomReconnectMs returns a uniform random int in [1000, 15000].
func randomReconnectMs() int {
	var b [2]byte
	_, _ = rand.Read(b[:])
	// Map to [1000, 15000].
	span := 15000 - 1000 + 1
	v := int(b[0])<<8 | int(b[1])
	return 1000 + (v % span)
}
