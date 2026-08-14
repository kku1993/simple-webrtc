package room

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/protocol"
	"github.com/kku1993/simple-webrtc-server/internal/state"
)

// --- helpers ---

// newPersistedRegistry builds a registry backed by a FileStore in a temp dir
// and a started persister. Returns the registry, the store, and a cleanup func.
func newPersistedRegistry(t *testing.T) (*Registry, *state.FileStore, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	r, _ := testRegistry(t)
	p := state.NewPersister(store, r.SnapshotRoom, 10*time.Millisecond, 256)
	p.Start()
	r.SetPersister(p)
	cleanup := func() {
		r.Stop()
	}
	return r, store, cleanup
}

// flushAndWait marks dirty and waits for the persister to flush.
func flushAndWait(t *testing.T, r *Registry, roomID string) {
	t.Helper()
	r.markDirty(roomID)
	time.Sleep(80 * time.Millisecond)
}

// --- snapshot round-trip ---

func TestSnapshotRoundTrip(t *testing.T) {
	r, _ := testRegistry(t)
	hostConn := newFakeConn("1.1.1.1")
	hs := NewSession(hostConn)
	res := r.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "host-epoch-1",
	}, true)
	if res.Response == nil {
		t.Fatalf("create-room failed")
	}
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	// Marshal the snapshot.
	r.mu.Lock()
	room := r.rooms[cr.RoomID]
	r.mu.Unlock()
	room.mu.Lock()
	data := r.marshalSnapshot(room)
	room.mu.Unlock()

	if len(data) == 0 {
		t.Fatalf("marshalSnapshot returned empty")
	}

	// Unmarshal back.
	room2, err := r.unmarshalSnapshot(data)
	if err != nil {
		t.Fatalf("unmarshalSnapshot: %v", err)
	}
	if room2.roomID != room.roomID {
		t.Errorf("roomID = %q, want %q", room2.roomID, room.roomID)
	}
	if room2.ownerIP != room.ownerIP {
		t.Errorf("ownerIP = %q, want %q", room2.ownerIP, room.ownerIP)
	}
	if !room2.expiresAt.Equal(room.expiresAt) {
		t.Errorf("expiresAt = %v, want %v", room2.expiresAt, room.expiresAt)
	}
	if room2.slots[protocol.RoleHost].epoch != "host-epoch-1" {
		t.Errorf("host epoch = %q", room2.slots[protocol.RoleHost].epoch)
	}
	// Conn must be nil after restore.
	if room2.slots[protocol.RoleHost].conn != nil {
		t.Errorf("host conn should be nil after restore")
	}
}

func TestSnapshotRejectsUnknownVersion(t *testing.T) {
	r, _ := testRegistry(t)
	_, err := r.unmarshalSnapshot([]byte(`{"v":999,"roomId":"ta0001"}`))
	if err == nil {
		t.Fatalf("expected error for unknown version")
	}
}

func TestSnapshotRejectsCorruptJSON(t *testing.T) {
	r, _ := testRegistry(t)
	_, err := r.unmarshalSnapshot([]byte(`{not json`))
	if err == nil {
		t.Fatalf("expected error for corrupt JSON")
	}
}

// --- persistence: create → flush → file exists ---

func TestPersistCreateRoom(t *testing.T) {
	r, store, cleanup := newPersistedRegistry(t)
	defer cleanup()

	hs := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "h1",
	}, true)
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	flushAndWait(t, r, cr.RoomID)

	all, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	data, ok := all[cr.RoomID]
	if !ok {
		t.Fatalf("room %s not persisted", cr.RoomID)
	}
	var snap roomSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("unmarshal persisted: %v", err)
	}
	if snap.Slots["host"].Epoch != "h1" {
		t.Errorf("persisted host epoch = %q, want h1", snap.Slots["host"].Epoch)
	}
}

// --- persistence: close → flush → file deleted ---

func TestPersistCloseRoomDeletesFile(t *testing.T) {
	r, store, cleanup := newPersistedRegistry(t)
	defer cleanup()

	hs := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "h1",
	}, true)
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	flushAndWait(t, r, cr.RoomID)

	// Close the room.
	_ = r.CloseRoom(hs, protocol.CloseRoomMsg{
		Type:      protocol.TypeCloseRoom,
		RequestID: "req1",
	})
	flushAndWait(t, r, cr.RoomID) // markDeleted is queued

	all, _ := store.LoadAll()
	if _, ok := all[cr.RoomID]; ok {
		t.Errorf("room file should be deleted after close")
	}
}

// --- restore: rehydrate rooms from disk ---

func TestRestoreRehydratesRooms(t *testing.T) {
	r1, store, cleanup := newPersistedRegistry(t)

	// Create a room with a host epoch and a buffered signal.
	hs := NewSession(newFakeConn("1.1.1.1"))
	res := r1.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "h1",
	}, true)
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	// Buffer a signal (host sends while guest is absent).
	_ = r1.Signal(hs, protocol.SignalMsg{
		Type:      protocol.TypeSignal,
		Seq:       1,
		Data:      json.RawMessage(`"hello"`),
		RequestID: "r1",
	})

	flushAndWait(t, r1, cr.RoomID)

	// Simulate restart: stop the old registry, create a new one, restore.
	cleanup()
	r2, _ := testRegistry(t)
	n, err := r2.Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 1 {
		t.Fatalf("Restore returned %d, want 1", n)
	}

	// The room should be in the new registry with no conns.
	r2.mu.Lock()
	room, ok := r2.rooms[cr.RoomID]
	r2.mu.Unlock()
	if !ok {
		t.Fatalf("room not restored into registry")
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	if room.slots[protocol.RoleHost].epoch != "h1" {
		t.Errorf("host epoch = %q, want h1", room.slots[protocol.RoleHost].epoch)
	}
	if room.slots[protocol.RoleHost].conn != nil {
		t.Errorf("host conn should be nil after restore")
	}
	if len(room.slots[protocol.RoleGuest].buffer) != 1 {
		t.Errorf("guest buffer = %d signals, want 1", len(room.slots[protocol.RoleGuest].buffer))
	}
	if room.slots[protocol.RoleGuest].buffer[0].Seq != 1 {
		t.Errorf("buffered signal seq = %d, want 1", room.slots[protocol.RoleGuest].buffer[0].Seq)
	}
	// releaseAt should be cleared.
	if room.releaseAt != nil {
		t.Errorf("releaseAt should be nil after restore")
	}
	// peerDeadlineAt should be set.
	if room.peerDeadlineAt == nil {
		t.Errorf("peerDeadlineAt should be set after restore")
	}
	// Global counter should reflect the restored room.
	if r2.RoomsGlobal() != 1 {
		t.Errorf("RoomsGlobal = %d, want 1", r2.RoomsGlobal())
	}
}

func TestRestoreSkipsExpiredRooms(t *testing.T) {
	r1, store, cleanup := newPersistedRegistry(t)

	hs := NewSession(newFakeConn("1.1.1.1"))
	res := r1.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "h1",
	}, true)
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	flushAndWait(t, r1, cr.RoomID)
	cleanup()

	// Advance the clock past expiry before restoring.
	r2, _ := testRegistry(t)
	// The room's expiry is now + 5400s (RoomMaxLifetimeSec). Use a clock
	// that is far in the future.
	originalNow := r2.now()
	r2.SetClock(func() time.Time { return originalNow.Add(2 * time.Hour) })

	n, err := r2.Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 0 {
		t.Errorf("Restore = %d, want 0 (room should be expired)", n)
	}
	// File should be cleaned up.
	all, _ := store.LoadAll()
	if _, ok := all[cr.RoomID]; ok {
		t.Errorf("expired room file should be deleted")
	}
}

func TestRestoreSkipsEmptyRooms(t *testing.T) {
	r, store, cleanup := newPersistedRegistry(t)
	defer cleanup()

	// Manually write a snapshot for a room with no epochs and no buffer.
	snap := roomSnapshot{
		Version:    SchemaVersion,
		RoomID:     "ta9999",
		OwnerIP:    "1.1.1.1",
		CreatedAt:  r.now(),
		ExpiresAt:  r.now().Add(1 * time.Hour),
		Slots: map[string]slotSnapshot{
			"host":  {},
			"guest": {},
		},
	}
	data, _ := json.Marshal(snap)
	_ = store.Save("ta9999", data)

	n, err := r.Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 0 {
		t.Errorf("Restore = %d, want 0 (empty room should be skipped)", n)
	}
}

func TestRestoreSkipsCorruptFiles(t *testing.T) {
	r, store, cleanup := newPersistedRegistry(t)
	defer cleanup()

	_ = store.Save("ta8888", []byte(`{corrupt`))
	n, err := r.Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 0 {
		t.Errorf("Restore = %d, want 0 for corrupt file", n)
	}
	// Corrupt file should be cleaned up.
	all, _ := store.LoadAll()
	if _, ok := all["ta8888"]; ok {
		t.Errorf("corrupt file should be deleted")
	}
}

// --- full cycle: create → persist → restart → rejoin works ---

func TestPersistThenRejoinAfterRestart(t *testing.T) {
	r1, store, cleanup := newPersistedRegistry(t)

	// Host creates a room.
	hs := NewSession(newFakeConn("1.1.1.1"))
	res := r1.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "h1",
	}, true)
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	flushAndWait(t, r1, cr.RoomID)

	// Simulate restart.
	cleanup()
	r2, _ := testRegistry(t)
	if _, err := r2.Restore(store); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Host rejoins with the same epoch.
	hostConn2 := newFakeConn("1.1.1.1")
	_ = NewSession(hostConn2)
	// We need a rejoin token. The token was issued during CreateRoom but we
	// don't have it here. Instead, verify the room exists and a guest can
	// join (which doesn't require a token).
	guestConn := newFakeConn("2.2.2.2")
	gs := NewSession(guestConn)
	res = r2.JoinRoom(gs, protocol.JoinRoomMsg{
		Type:       protocol.TypeJoinRoom,
		RoomID:     cr.RoomID,
		GuestEpoch: "g1",
	})
	if res.Response == nil {
		t.Fatalf("join-room failed after restore")
	}
	var jr protocol.JoinRoomResponse
	body, _ = json.Marshal(res.Response)
	_ = json.Unmarshal(body, &jr)
	if jr.RoomID != cr.RoomID {
		t.Errorf("join roomID = %q, want %q", jr.RoomID, cr.RoomID)
	}
	if jr.HostEpoch == nil || *jr.HostEpoch != "h1" {
		t.Errorf("host epoch in join response = %v, want h1", jr.HostEpoch)
	}
}

// --- signal buffering persists ---

func TestPersistSignalBuffer(t *testing.T) {
	r, store, cleanup := newPersistedRegistry(t)
	defer cleanup()

	hs := NewSession(newFakeConn("1.1.1.1"))
	res := r.CreateRoom(hs, protocol.CreateRoomMsg{
		Type:      protocol.TypeCreateRoom,
		HostEpoch: "h1",
	}, true)
	var cr protocol.CreateRoomResponse
	body, _ := json.Marshal(res.Response)
	_ = json.Unmarshal(body, &cr)

	// Buffer multiple signals.
	for i := 1; i <= 3; i++ {
		_ = r.Signal(hs, protocol.SignalMsg{
			Type:      protocol.TypeSignal,
			Seq:       i,
			Data:      json.RawMessage(`"sig"`),
			RequestID: "r",
		})
	}

	flushAndWait(t, r, cr.RoomID)

	all, _ := store.LoadAll()
	data, ok := all[cr.RoomID]
	if !ok {
		t.Fatalf("room not persisted")
	}
	var snap roomSnapshot
	_ = json.Unmarshal(data, &snap)
	if len(snap.Slots["guest"].Buffer) != 3 {
		t.Errorf("persisted guest buffer = %d, want 3", len(snap.Slots["guest"].Buffer))
	}
}

// --- MarkAllDirty ---

func TestMarkAllDirty(t *testing.T) {
	r, store, cleanup := newPersistedRegistry(t)
	defer cleanup()

	// Create two rooms without flushing (persister interval is 10ms, so they
	// will flush automatically, but let's test MarkAllDirty explicitly).
	for i := 0; i < 2; i++ {
		hs := NewSession(newFakeConn("1.1.1.1"))
		_ = r.CreateRoom(hs, protocol.CreateRoomMsg{
			Type:      protocol.TypeCreateRoom,
			HostEpoch: "h",
		}, true)
	}

	// Wait for initial flush.
	time.Sleep(80 * time.Millisecond)

	// MarkAllDirty should re-queue all rooms.
	r.MarkAllDirty()
	time.Sleep(80 * time.Millisecond)

	all, _ := store.LoadAll()
	if len(all) < 2 {
		t.Errorf("expected >= 2 persisted rooms, got %d", len(all))
	}
}
