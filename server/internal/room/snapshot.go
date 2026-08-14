package room

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/protocol"
	"github.com/kku1993/simple-webrtc-server/internal/state"
)

// This file implements serialization of Room state to/from JSON for disk
// persistence (internal/state). Only the durable, connection-independent
// fields are serialized; live Conn pointers are dropped on restore and the
// clients are expected to rejoin to re-establish their slots.
//
// The snapshot format is versioned (SchemaVersion) so future format changes
// can be detected and handled without corrupting live rooms.

// SchemaVersion is the persistence format version. Bump when the on-disk
// shape changes in an incompatible way; restoreRoom rejects unknown versions.
const SchemaVersion = 1

// slotSnapshot is the serializable projection of a Slot. The Conn field is
// omitted — it is a live network handle and cannot survive a restart.
type slotSnapshot struct {
	Epoch             string          `json:"epoch,omitempty"`
	ReportedConnected bool            `json:"reportedConnected,omitempty"`
	LastSeq           int             `json:"lastSeq,omitempty"`
	Buffer            []bufferedSignal `json:"buffer,omitempty"`
	BufferBytes       int             `json:"bufferBytes,omitempty"`
	OverflowReset     bool            `json:"overflowReset,omitempty"`
}

// roomSnapshot is the serializable projection of a Room.
type roomSnapshot struct {
	Version            int                  `json:"v"`
	RoomID             string               `json:"roomId"`
	CreatedAt          time.Time            `json:"createdAt"`
	InstantiatedAt     time.Time            `json:"instantiatedAt"`
	ExpiresAt          time.Time            `json:"expiresAt"`
	PeerDeadlineAt     *time.Time           `json:"peerDeadlineAt,omitempty"`
	GuestPasswordHash  []byte               `json:"guestPasswordHash,omitempty"`
	GuestPasswordSalt  []byte               `json:"guestPasswordSalt,omitempty"`
	PasswordAttempts   int                  `json:"passwordAttempts,omitempty"`
	OwnerIP            string               `json:"ownerIP"`
	Slots              map[string]slotSnapshot `json:"slots"`
	TurnCache          []protocol.IceServer `json:"turnCache,omitempty"`
	TurnCacheExpiresAt time.Time            `json:"turnCacheExpiresAt,omitempty"`
}

// marshalSnapshot serializes the durable fields of room into JSON bytes. The
// caller must hold room.mu.
func (r *Registry) marshalSnapshot(room *Room) []byte {
	snap := roomSnapshot{
		Version:          SchemaVersion,
		RoomID:           room.roomID,
		CreatedAt:        room.createdAt,
		InstantiatedAt:   room.instantiatedAt,
		ExpiresAt:        room.expiresAt,
		PeerDeadlineAt:   room.peerDeadlineAt,
		GuestPasswordHash: room.guestPasswordHash,
		GuestPasswordSalt: room.guestPasswordSalt,
		PasswordAttempts:  room.passwordAttempts,
		OwnerIP:          room.ownerIP,
		Slots:            make(map[string]slotSnapshot, 2),
		TurnCache:        room.turnCache,
		TurnCacheExpiresAt: room.turnCacheExpiresAt,
	}
	for role, slot := range room.slots {
		snap.Slots[string(role)] = slotSnapshot{
			Epoch:             slot.epoch,
			ReportedConnected: slot.reportedConnected,
			LastSeq:           slot.lastSeq,
			Buffer:            slot.buffer,
			BufferBytes:       slot.bufferBytes,
			OverflowReset:     slot.overflowReset,
		}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		// json.Marshal of a plain struct only fails on unsupported types;
		// log and return nil so the persister skips this room.
		return nil
	}
	return data
}

// unmarshalSnapshot deserializes JSON bytes into a Room. The returned Room has
// nil Conn fields in both slots; callers must attach sessions via the normal
// handshake/rejoin path. The Room's reg pointer is set to r so lifecycle
// helpers (removeRoomLocked etc.) work.
func (r *Registry) unmarshalSnapshot(data []byte) (*Room, error) {
	var snap roomSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("room: unmarshal snapshot: %w", err)
	}
	if snap.Version != SchemaVersion {
		return nil, fmt.Errorf("room: unknown snapshot version %d (want %d)", snap.Version, SchemaVersion)
	}
	room := &Room{
		reg:               r,
		roomID:            snap.RoomID,
		createdAt:         snap.CreatedAt,
		instantiatedAt:    snap.InstantiatedAt,
		expiresAt:         snap.ExpiresAt,
		peerDeadlineAt:    snap.PeerDeadlineAt,
		guestPasswordHash: snap.GuestPasswordHash,
		guestPasswordSalt: snap.GuestPasswordSalt,
		passwordAttempts:  snap.PasswordAttempts,
		ownerIP:           snap.OwnerIP,
		slots:             make(map[protocol.Role]*Slot, 2),
		turnCache:         snap.TurnCache,
		turnCacheExpiresAt: snap.TurnCacheExpiresAt,
	}
	for _, role := range []protocol.Role{protocol.RoleHost, protocol.RoleGuest} {
		ss, ok := snap.Slots[string(role)]
		if !ok {
			room.slots[role] = &Slot{}
			continue
		}
		room.slots[role] = &Slot{
			epoch:             ss.Epoch,
			reportedConnected: ss.ReportedConnected,
			lastSeq:           ss.LastSeq,
			buffer:            ss.Buffer,
			bufferBytes:       ss.BufferBytes,
			overflowReset:     ss.OverflowReset,
		}
	}
	return room, nil
}

// Restore loads all room state files from store and rehydrates live rooms into
// the registry. Rooms whose expiry has already passed are skipped (and their
// files deleted so the directory does not grow unbounded). For each restored
// room:
//
//   - Conn fields are nil (clients must rejoin to re-establish slots).
//   - releaseAt is cleared: the peers may or may not still be P2P-connected
//     after the restart, so clearing the grace-release flag gives them a
//     rejoin window instead of immediately reaping the room.
//   - A fresh peer deadline is set from now (bounded by expiresAt) so the
//     room is reaped if no client rejoins within the configured window.
//   - The global rooms counter and per-IP counter are incremented to reflect
//     the restored room.
//
// Returns the number of rooms restored. Logs (does not return) errors for
// individual corrupt files; a failure to read the directory is returned.
func (r *Registry) Restore(store state.Store) (int, error) {
	all, err := store.LoadAll()
	if err != nil {
		return 0, fmt.Errorf("room: restore: %w", err)
	}
	now := r.now()
	restored := 0
	for roomID, data := range all {
		room, err := r.unmarshalSnapshot(data)
		if err != nil {
			log.Printf("room: restore: skip %s: %v", roomID, err)
			_ = store.Delete(roomID)
			continue
		}
		// Skip rooms that are already past their expiry; clean up the file.
		if !now.Before(room.expiresAt) {
			_ = store.Delete(roomID)
			continue
		}
		// Skip rooms with no recorded epochs and no buffered signals — there
		// is nothing to rejoin to.
		host := room.slots[protocol.RoleHost]
		guest := room.slots[protocol.RoleGuest]
		if host.epoch == "" && guest.epoch == "" &&
			len(host.buffer) == 0 && len(guest.buffer) == 0 {
			_ = store.Delete(roomID)
			continue
		}

		// Clear releaseAt (peers may need to rejoin) and set a fresh peer
		// deadline from now so the room is reaped if nobody rejoins.
		room.releaseAt = nil
		deadline := r.computePeerDeadline(now, room.expiresAt)
		room.peerDeadlineAt = &deadline

		// Restore counters.
		r.roomsGlobal.Add(1)
		r.metrics.SetLiveRooms(r.roomsGlobal.Load())
		if _, ok := r.roomsPerIP.Increment(room.ownerIP); !ok {
			// Per-IP counter is full; the room is still restored (the
			// counter is a soft limit for new creates, not a hard
			// invariant for restored rooms).
			log.Printf("room: restore: per-IP counter full for %s", room.ownerIP)
		}

		r.mu.Lock()
		r.rooms[room.roomID] = room
		r.mu.Unlock()
		restored++
	}
	if restored > 0 {
		log.Printf("room: restored %d room(s) from disk", restored)
	}
	return restored, nil
}
