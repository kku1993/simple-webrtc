package room

import (
	"encoding/base64"
	"time"

	"github.com/kku1993/simple-peer-signal-server/internal/protocol"
	"github.com/kku1993/simple-peer-signal-server/internal/tombstone"
	"github.com/kku1993/simple-peer-signal-server/internal/token"
)

// Result is what a handler returns to the server layer.
//
// Response, if non-nil, is marshaled and sent to the client. CloseCode, if
// non-nil, causes the server to close the connection after sending Response.
// A handler may set CloseCode without a Response (e.g. handshake timeout).
type Result struct {
	Response  any
	CloseCode *int
}

// closeWith returns a Result that closes the connection after sending resp (if any).
func closeWith(resp any, code int) Result {
	c := code
	return Result{Response: resp, CloseCode: &c}
}

// errResult builds an error-response Result.
func errResult(code protocol.ErrorCode, msg, requestID string, retryAfterMs *int) Result {
	return Result{Response: protocol.NewError(code, msg, requestID, retryAfterMs)}
}

// fatalErrResult builds an error-response Result that also closes the connection.
func fatalErrResult(closeCode int, code protocol.ErrorCode, msg, requestID string, retryAfterMs *int) Result {
	c := closeCode
	return Result{
		Response:  protocol.NewError(code, msg, requestID, retryAfterMs),
		CloseCode: &c,
	}
}

func msPtr(d time.Duration) *int {
	ms := int(d.Milliseconds())
	if ms <= 0 {
		ms = 1
	}
	return &ms
}

// CreateRoom handles a create-room message.
func (r *Registry) CreateRoom(s *Session, m protocol.CreateRoomMsg, turnstileOK bool) Result {
	if s.attached {
		return fatalErrResult(int(protocol.CloseProtocolError), protocol.ErrUnexpectedState,
			"connection already attached", m.RequestID, nil)
	}
	if m.HostEpoch == "" || len(m.HostEpoch) > MaxEpochLen {
		return errResult(protocol.ErrMalformedMessage, "hostEpoch required (max 64 chars)", m.RequestID, nil)
	}
	if r.cfg.TurnstileSecretKey != "" && !turnstileOK {
		// Distinguish "missing" from "invalid": the server layer sets
		// turnstileOK=false for both; the client must obtain a fresh token
		// either way. We return TURNSTILE_REQUIRED if no token was supplied
		// and TURNSTILE_INVALID if a token was supplied but rejected.
		if m.CloudflareTurnstileToken == "" {
			return errResult(protocol.ErrTurnstileRequired, "turnstile token required", m.RequestID, nil)
		}
		return errResult(protocol.ErrTurnstileInvalid, "turnstile token invalid", m.RequestID, nil)
	}

	ip := s.conn.IP()

	// Per-IP create-room rate limit (also covers recreating rejoins).
	if ok, wait := r.createLimiter.Allow(ip); !ok {
		r.metrics.RateLimitRejects.Inc()
		return errResult(protocol.ErrRateLimited, "create-room rate limit", m.RequestID, msPtr(wait))
	}

	// Per-IP concurrent rooms.
	if _, ok := r.roomsPerIP.Increment(ip); !ok {
		r.metrics.RateLimitRejects.Inc()
		return errResult(protocol.ErrRateLimited, "per-IP room limit reached", m.RequestID, nil)
	}

	// Global rooms cap.
	if !r.acquireGlobalRoomSlot() {
		r.roomsPerIP.Decrement(ip)
		r.metrics.RateLimitRejects.Inc()
		return errResult(protocol.ErrServerAtCapacity, "server at capacity", m.RequestID, msPtr(30*time.Second))
	}

	roomID, err := r.generateRoomID()
	if err != nil {
		r.roomsPerIP.Decrement(ip)
		r.releaseGlobalRoomSlot()
		return errResult(protocol.ErrServerAtCapacity, "could not allocate room id", m.RequestID, nil)
	}

	now := r.now()
	room := &Room{
		reg:             r,
		roomID:          roomID,
		createdAt:       now,
		instantiatedAt:  now,
		expiresAt:       r.computeExpiresAt(now, now),
		ownerIP:         ip,
		slots: map[protocol.Role]*Slot{
			protocol.RoleHost:  {},
			protocol.RoleGuest: {},
		},
	}
	deadline := r.computePeerDeadline(now, room.expiresAt)
	room.peerDeadlineAt = &deadline

	if m.GuestPassword != "" {
		salt := genSalt()
		hashB64, saltB64 := hashPassword(salt, m.GuestPassword)
		// Store raw bytes for constant-time compare.
		room.guestPasswordHash = mustDecodeB64(hashB64)
		room.guestPasswordSalt = mustDecodeB64(saltB64)
	}

	// Attach host slot.
	room.slots[protocol.RoleHost].conn = s.conn
	room.slots[protocol.RoleHost].epoch = m.HostEpoch

	r.mu.Lock()
	r.rooms[roomID] = room
	r.mu.Unlock()

	s.mu.Lock()
	s.room = room
	s.role = protocol.RoleHost
	s.attached = true
	s.mu.Unlock()

	r.metrics.RoomsCreated.Inc()

	// Issue rejoin token.
	tok, err := r.signer.Issue(token.Payload{
		V:                 1,
		RoomID:            roomID,
		Role:              string(protocol.RoleHost),
		CreatedAt:         now.Unix(),
		GuestPasswordHash: optionalB64(room.guestPasswordHash),
		GuestPasswordSalt: optionalB64(room.guestPasswordSalt),
	})
	if err != nil {
		// Should not happen with a valid secret; treat as server error.
		return errResult(protocol.ErrServerAtCapacity, "could not issue token", m.RequestID, nil)
	}

	resp := protocol.CreateRoomResponse{
		Type:                  protocol.TypeCreateRoomResponse,
		RequestID:             m.RequestID,
		RoomID:                roomID,
		Role:                  protocol.RoleHost,
		RejoinToken:           tok,
		PeerDeadlineAt:        isoTime(deadline),
		PeerDeadlineInSeconds: secondsUntil(now, deadline),
		RoomExpiresAt:         isoTime(room.expiresAt),
		RoomExpiresInSeconds:  secondsUntil(now, room.expiresAt),
		RejoinTokenExpiresAt:  isoTime(room.createdAt.Add(r.cfg.RejoinTokenTtl())),
	}
	return Result{Response: resp}
}

// JoinRoom handles a join-room message.
func (r *Registry) JoinRoom(s *Session, m protocol.JoinRoomMsg) Result {
	if s.attached {
		return fatalErrResult(int(protocol.CloseProtocolError), protocol.ErrUnexpectedState,
			"connection already attached", m.RequestID, nil)
	}
	if m.RoomID == "" || len(m.RoomID) > MaxRoomIDLen {
		return errResult(protocol.ErrMalformedMessage, "roomId required (max 64 chars)", m.RequestID, nil)
	}
	if m.GuestEpoch == "" || len(m.GuestEpoch) > MaxEpochLen {
		return errResult(protocol.ErrMalformedMessage, "guestEpoch required (max 64 chars)", m.RequestID, nil)
	}

	// Tombstone check first → ROOM_CLOSED is more specific than ROOM_NOT_FOUND.
	if r.tomb.Has(m.RoomID) {
		return errResult(protocol.ErrRoomClosed, "room is closed", m.RequestID, nil)
	}

	r.mu.Lock()
	room, ok := r.rooms[m.RoomID]
	r.mu.Unlock()
	if !ok {
		return errResult(protocol.ErrRoomNotFound, "room not found", m.RequestID, nil)
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	// Password check.
	if room.guestPasswordHash != nil {
		if !verifyPassword(room.guestPasswordSalt, room.guestPasswordHash, m.GuestPassword) {
			room.passwordAttempts++
			if room.passwordAttempts >= r.cfg.MaxPasswordAttempts {
				// Lockout: close the room permanently with a tombstone.
				r.closeRoomLocked(room, tombstone.ReasonPasswordLockout, "password lockout")
				return fatalErrResult(int(protocol.CloseRoomClosed), protocol.ErrTooManyPasswordAttempts,
					"too many password attempts", m.RequestID, nil)
			}
			return errResult(protocol.ErrInvalidGuestPassword, "invalid guest password", m.RequestID, nil)
		}
	}

	guestSlot := room.slots[protocol.RoleGuest]
	if guestSlot.conn != nil {
		return errResult(protocol.ErrRoomFull, "room is full", m.RequestID, nil)
	}

	// Attach guest.
	guestSlot.conn = s.conn
	guestSlot.epoch = m.GuestEpoch
	// Joining clears the peer deadline.
	room.peerDeadlineAt = nil

	hostSlot := room.slots[protocol.RoleHost]

	s.mu.Lock()
	s.room = room
	s.role = protocol.RoleGuest
	s.attached = true
	s.mu.Unlock()

	// Notify host.
	if hostSlot.conn != nil {
		send(hostSlot.conn, protocol.GuestJoinedEvent{
			Type:       protocol.TypeGuestJoined,
			GuestEpoch: m.GuestEpoch,
		})
	}

	// Issue guest rejoin token (same createdAt as the room).
	tok, err := r.signer.Issue(token.Payload{
		V:                 1,
		RoomID:            room.roomID,
		Role:              string(protocol.RoleGuest),
		CreatedAt:         room.createdAt.Unix(),
		GuestPasswordHash: optionalB64(room.guestPasswordHash),
		GuestPasswordSalt: optionalB64(room.guestPasswordSalt),
	})
	if err != nil {
		return errResult(protocol.ErrServerAtCapacity, "could not issue token", m.RequestID, nil)
	}

	now := r.now()
	resp := protocol.JoinRoomResponse{
		Type:                 protocol.TypeJoinRoomResponse,
		RequestID:            m.RequestID,
		RoomID:               room.roomID,
		Role:                 protocol.RoleGuest,
		RejoinToken:          tok,
		HostConnected:        hostSlot.conn != nil,
		HostEpoch:            ptrStr(hostSlot.epoch),
		GuestEpoch:           m.GuestEpoch,
		RoomExpiresAt:        isoTime(room.expiresAt),
		RoomExpiresInSeconds: secondsUntil(now, room.expiresAt),
		RejoinTokenExpiresAt: isoTime(room.createdAt.Add(r.cfg.RejoinTokenTtl())),
	}
	return Result{Response: resp}
}

// RejoinRoom handles a rejoin-room message, implementing the exact validation
// order from docs/DESIGN.md §"RejoinRoom".
func (r *Registry) RejoinRoom(s *Session, m protocol.RejoinRoomMsg) Result {
	if s.attached {
		return fatalErrResult(int(protocol.CloseProtocolError), protocol.ErrUnexpectedState,
			"connection already attached", m.RequestID, nil)
	}
	if m.RejoinToken == "" {
		return errResult(protocol.ErrInvalidRejoinToken, "missing token", m.RequestID, nil)
	}
	if m.Epoch == "" || len(m.Epoch) > MaxEpochLen {
		return errResult(protocol.ErrMalformedMessage, "epoch required (max 64 chars)", m.RequestID, nil)
	}

	// 1. Parse and verify the token.
	p, err := r.signer.Verify(m.RejoinToken)
	if err != nil {
		return errResult(protocol.ErrInvalidRejoinToken, "invalid token", m.RequestID, nil)
	}

	// 2. TTL check.
	now := r.now()
	if token.Expired(p, now, r.cfg.RejoinTokenTtl()) {
		return errResult(protocol.ErrRoomExpired, "token expired", m.RequestID, nil)
	}

	// 3. Tombstone check.
	if r.tomb.Has(p.RoomID) {
		return errResult(protocol.ErrRoomClosed, "room is closed", m.RequestID, nil)
	}

	role := protocol.Role(p.Role)

	// Recreates count against the same limits as creates.
	ip := s.conn.IP()
	if ok, wait := r.createLimiter.Allow(ip); !ok {
		r.metrics.RateLimitRejects.Inc()
		return errResult(protocol.ErrRateLimited, "rejoin rate limit", m.RequestID, msPtr(wait))
	}

	r.mu.Lock()
	room, ok := r.rooms[p.RoomID]
	r.mu.Unlock()

	recreated := false
	if !ok {
		// 4b. Recreate the room from the token.
		if _, ok := r.roomsPerIP.Increment(ip); !ok {
			r.metrics.RateLimitRejects.Inc()
			return errResult(protocol.ErrRateLimited, "per-IP room limit reached", m.RequestID, nil)
		}
		if !r.acquireGlobalRoomSlot() {
			r.roomsPerIP.Decrement(ip)
			r.metrics.RateLimitRejects.Inc()
			return errResult(protocol.ErrServerAtCapacity, "server at capacity", m.RequestID, msPtr(30*time.Second))
		}

		createdAt := time.Unix(p.CreatedAt, 0)
		room = &Room{
			reg:            r,
			roomID:         p.RoomID,
			createdAt:      createdAt,
			instantiatedAt: now,
			expiresAt:      r.computeExpiresAt(now, createdAt),
			ownerIP:        ip,
			slots: map[protocol.Role]*Slot{
				protocol.RoleHost:  {},
				protocol.RoleGuest: {},
			},
		}
		if p.GuestPasswordHash != "" && p.GuestPasswordSalt != "" {
			room.guestPasswordHash = mustDecodeB64(p.GuestPasswordHash)
			room.guestPasswordSalt = mustDecodeB64(p.GuestPasswordSalt)
		}
		deadline := r.computePeerDeadline(now, room.expiresAt)
		room.peerDeadlineAt = &deadline

		r.mu.Lock()
		r.rooms[p.RoomID] = room
		r.mu.Unlock()
		recreated = true
		r.metrics.RejoinsRecreated.Inc()
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	mySlot := room.slots[role]
	otherRole := role.Other()
	otherSlot := room.slots[otherRole]

	// 4a. If my slot is occupied, evict the old connection with 4400.
	if mySlot.conn != nil {
		old := mySlot.conn
		mySlot.conn = nil
		old.Close(int(protocol.CloseReplaced), "replaced by a newer connection")
	}

	// Determine epoch change for peer-rejoined/peer-reset decision.
	prevEpoch := mySlot.epoch
	epochChanged := recreated || prevEpoch != m.Epoch

	// Attach.
	mySlot.conn = s.conn
	mySlot.epoch = m.Epoch
	if epochChanged {
		// Reset buffer and lastSeq for the new epoch.
		mySlot.buffer = nil
		mySlot.bufferBytes = 0
		mySlot.lastSeq = 0
		mySlot.reportedConnected = false
		mySlot.overflowReset = false
	}

	// Manage peer deadline: if the other slot is empty, set one; else clear.
	if otherSlot.conn == nil {
		deadline := r.computePeerDeadline(now, room.expiresAt)
		room.peerDeadlineAt = &deadline
	} else {
		room.peerDeadlineAt = nil
	}

	s.mu.Lock()
	s.room = room
	s.role = role
	s.attached = true
	s.mu.Unlock()

	// Notify the other slot about this attach.
	if otherSlot.conn != nil {
		if epochChanged {
			// peer-reset: the other side must rebuild. Also clear the other
			// slot's buffer (it holds signals from our old epoch).
			otherSlot.buffer = nil
			otherSlot.bufferBytes = 0
			otherSlot.lastSeq = 0
			otherSlot.reportedConnected = false
			otherSlot.overflowReset = false
			send(otherSlot.conn, protocol.PeerResetEvent{
				Type:  protocol.TypePeerReset,
				Role:  role,
				Epoch: m.Epoch,
			})
		} else {
			send(otherSlot.conn, protocol.PeerRejoinedEvent{
				Type:  protocol.TypePeerRejoined,
				Role:  role,
				Epoch: m.Epoch,
			})
		}
	}

	// Replay buffered signals to the rejoining slot if epoch unchanged.
	if !epochChanged && len(mySlot.buffer) > 0 {
		for _, bs := range mySlot.buffer {
			send(mySlot.conn, protocol.SignalResponse{
				Type:       protocol.TypeSignalResponse,
				FromRole:   bs.FromRole,
				FromEpoch:  bs.FromEpoch,
				Seq:        bs.Seq,
				Data:       bs.Data,
				ReceivedAt: isoTime(bs.ReceivedAt),
			})
		}
	}

	if !recreated {
		r.metrics.RejoinsSame.Inc()
	}

	resp := protocol.RejoinRoomResponse{
		Type:                 protocol.TypeRejoinRoomResponse,
		RequestID:            m.RequestID,
		RoomID:               room.roomID,
		Role:                 role,
		Recreated:            recreated,
		PeerConnected:        otherSlot.conn != nil,
		HostEpoch:            ptrStr(room.slots[protocol.RoleHost].epoch),
		GuestEpoch:           ptrStr(room.slots[protocol.RoleGuest].epoch),
		RoomExpiresAt:        isoTime(room.expiresAt),
		RoomExpiresInSeconds: secondsUntil(now, room.expiresAt),
		RejoinTokenExpiresAt: isoTime(room.createdAt.Add(r.cfg.RejoinTokenTtl())),
	}
	if otherSlot.conn == nil && room.peerDeadlineAt != nil {
		d := *room.peerDeadlineAt
		secs := secondsUntil(now, d)
		resp.PeerDeadlineAt = strPtr(isoTime(d))
		resp.PeerDeadlineInSeconds = &secs
	}
	return Result{Response: resp}
}

// Signal handles a signal message: forward to the other slot, or buffer it.
func (r *Registry) Signal(s *Session, m protocol.SignalMsg) Result {
	if !s.attached {
		return errResult(protocol.ErrUnexpectedState, "not attached to a room", m.RequestID, nil)
	}
	if m.Seq <= 0 {
		return errResult(protocol.ErrMalformedMessage, "seq must be positive", m.RequestID, nil)
	}

	s.mu.Lock()
	room := s.room
	role := s.role
	s.mu.Unlock()

	room.mu.Lock()
	defer room.mu.Unlock()

	mySlot := room.slots[role]
	otherRole := role.Other()
	otherSlot := room.slots[otherRole]

	// Duplicate suppression: drop seq <= lastSeq for the current epoch.
	if m.Seq <= mySlot.lastSeq {
		return Result{} // silently ignore
	}
	mySlot.lastSeq = m.Seq

	now := r.now()
	r.metrics.SignalsRelayed.Inc()
	r.metrics.BytesRelayed.Add(float64(len(m.Data)))

	if otherSlot.conn != nil {
		ok := send(otherSlot.conn, protocol.SignalResponse{
			Type:       protocol.TypeSignalResponse,
			FromRole:   role,
			FromEpoch:  mySlot.epoch,
			Seq:        m.Seq,
			Data:       m.Data,
			ReceivedAt: isoTime(now),
		})
		_ = ok
		return Result{}
	}

	// Buffer for the unoccupied other slot.
	// Overflow check: count and bytes.
	if len(otherSlot.buffer) >= r.cfg.MaxBufferedSignals ||
		otherSlot.bufferBytes+len(m.Data) > r.cfg.MaxBufferedSignalBytes {
		// Overflow: tell the sender, clear the buffer, mark the receiver as
		// needing reset.
		otherSlot.buffer = nil
		otherSlot.bufferBytes = 0
		otherSlot.overflowReset = true
		r.metrics.SignalBufferOverflow.Inc()
		return errResult(protocol.ErrSignalBufferOverflow, "signal buffer overflow", m.RequestID, nil)
	}

	otherSlot.buffer = append(otherSlot.buffer, bufferedSignal{
		Seq:        m.Seq,
		FromRole:   role,
		FromEpoch:  mySlot.epoch,
		Data:       m.Data,
		ReceivedAt: now,
	})
	otherSlot.bufferBytes += len(m.Data)
	return Result{}
}

// PeerConnected handles a peer-connected message.
func (r *Registry) PeerConnected(s *Session, m protocol.PeerConnectedMsg) Result {
	if !s.attached {
		return errResult(protocol.ErrUnexpectedState, "not attached to a room", m.RequestID, nil)
	}
	s.mu.Lock()
	room := s.room
	role := s.role
	s.mu.Unlock()

	room.mu.Lock()
	defer room.mu.Unlock()

	mySlot := room.slots[role]
	if mySlot.reportedConnected {
		return Result{} // idempotent
	}
	mySlot.reportedConnected = true

	otherSlot := room.slots[role.Other()]
	if otherSlot.reportedConnected && r.cfg.ReleaseSocketsOnPeerConnected {
		// Both connected; schedule release after grace.
		releaseAt := r.now().Add(r.cfg.PeerConnectedGrace())
		room.releaseAt = &releaseAt
	}
	return Result{}
}

// CloseRoom handles a close-room message.
func (r *Registry) CloseRoom(s *Session, m protocol.CloseRoomMsg) Result {
	if !s.attached {
		return errResult(protocol.ErrUnexpectedState, "not attached to a room", m.RequestID, nil)
	}
	s.mu.Lock()
	room := s.room
	s.mu.Unlock()

	room.mu.Lock()
	r.closeRoomLocked(room, tombstone.ReasonClosedByPeer, "closed by peer")
	room.mu.Unlock()
	return Result{}
}

// closeRoomLocked writes a tombstone, notifies both slots, closes both sockets
// with 4013, and removes the room from the registry. The caller must hold
// room.mu.
func (r *Registry) closeRoomLocked(room *Room, reason tombstone.Reason, _ string) {
	r.tomb.Add(room.roomID, reason)
	for _, slot := range room.slots {
		if slot.conn != nil {
			send(slot.conn, protocol.RoomClosedEvent{
				Type:   protocol.TypeRoomClosed,
				Reason: string(reason),
			})
			slot.conn.Close(int(protocol.CloseRoomClosed), "room closed")
			slot.conn = nil
		}
	}
	r.removeRoomLocked(room)
}

// removeRoomLocked deletes the room from the registry and decrements the global
// and per-IP counters. The caller must hold room.mu (or be in a context where
// the room is not yet published).
func (r *Registry) removeRoomLocked(room *Room) {
	r.mu.Lock()
	if _, ok := r.rooms[room.roomID]; ok {
		delete(r.rooms, room.roomID)
		r.releaseGlobalRoomSlot()
		r.roomsPerIP.Decrement(room.ownerIP)
	}
	r.mu.Unlock()
}

// Detach is called when a connection's WebSocket has closed. It removes the
// connection from its slot (if it still owns it), notifies the other slot with
// peer-disconnected, and sets a peer deadline if exactly one slot remains.
func (r *Registry) Detach(s *Session) {
	s.mu.Lock()
	room := s.room
	role := s.role
	attached := s.attached
	s.room = nil
	s.role = ""
	s.attached = false
	s.mu.Unlock()

	if !attached || room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	mySlot := room.slots[role]
	// Only clear if this session still owns the slot (it may have been evicted
	// by a rejoin, in which case mySlot.conn is a different conn).
	if mySlot.conn == s.conn {
		mySlot.conn = nil
	}
	// Note: we keep mySlot.epoch, buffer, lastSeq so a same-epoch rejoin can
	// replay.

	otherSlot := room.slots[role.Other()]
	if otherSlot.conn != nil {
		send(otherSlot.conn, protocol.PeerDisconnectedEvent{
			Type: protocol.TypePeerDisconnected,
			Role: role,
		})
		// Set a peer deadline for the remaining slot.
		now := r.now()
		deadline := r.computePeerDeadline(now, room.expiresAt)
		room.peerDeadlineAt = &deadline
	} else {
		// Both slots empty; the room is abandoned. Leave it in the map for the
		// peer deadline / expiry sweep to reap, OR remove now if no buffer
		// remains and no epoch recorded (truly empty). Per the design, a room
		// with zero occupied slots still exists until expiry or peer deadline;
		// rejoin tokens can resurrect it. Keep it.
		// However, if neither slot ever recorded an epoch, there's nothing to
		// rejoin to — drop it.
		if room.slots[protocol.RoleHost].epoch == "" && room.slots[protocol.RoleGuest].epoch == "" {
			r.removeRoomLocked(room)
		} else {
			now := r.now()
			deadline := r.computePeerDeadline(now, room.expiresAt)
			room.peerDeadlineAt = &deadline
		}
	}
}

// --- helpers used by handlers ---

func (r *Registry) acquireGlobalRoomSlot() bool {
	for {
		cur := r.roomsGlobal.Load()
		if cur >= int64(r.cfg.MaxRoomsGlobal) {
			return false
		}
		if r.roomsGlobal.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (r *Registry) releaseGlobalRoomSlot() {
	r.roomsGlobal.Add(-1)
}

// computeExpiresAt = min(instantiatedAt + roomMaxLifetime, createdAt + tokenTtl).
func (r *Registry) computeExpiresAt(instantiatedAt, createdAt time.Time) time.Time {
	a := instantiatedAt.Add(r.cfg.RoomMaxLifetime())
	b := createdAt.Add(r.cfg.RejoinTokenTtl())
	if b.Before(a) {
		return b
	}
	return a
}

// computePeerDeadline = min(now + peerDeadline, room.expiresAt).
func (r *Registry) computePeerDeadline(now, expiresAt time.Time) time.Time {
	a := now.Add(r.cfg.PeerDeadline())
	if expiresAt.Before(a) {
		return expiresAt
	}
	return a
}

func optionalB64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return b64Std(b)
}

func b64Std(b []byte) string         { return base64.StdEncoding.EncodeToString(b) }
func mustDecodeB64(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

func strPtr(s string) *string { return &s }
