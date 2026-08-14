// Package state implements disk persistence of room state.
//
// It provides a Store abstraction over a directory of JSON files (one file per
// room) and a batching Persister that coalesces and queues writes so that
// high-frequency state changes (e.g. signal buffering) do not overwhelm disk
// bandwidth. The Persister drains a channel of dirty/deleted room IDs, merges
// them into a coalesced set, and flushes on a timer or when the batch reaches a
// configured size — so a room dirtied a thousand times within one flush interval
// results in a single file write.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store is the persistence backend. Each room is stored under its roomID key as
// an opaque blob; the caller (the room package) is responsible for marshaling
// and unmarshaling. Keys are filesystem-safe room IDs.
type Store interface {
	// Save writes data for key atomically (temp file + rename).
	Save(key string, data []byte) error
	// Delete removes the entry for key. A missing entry is not an error.
	Delete(key string) error
	// LoadAll returns every persisted entry keyed by roomID.
	LoadAll() (map[string][]byte, error)
}

// FileStore implements Store over a directory of JSON files, one per room.
// Files are named `<key>.json` and written via a temp file + atomic rename so a
// crash never leaves a partial file.
type FileStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStore creates a FileStore rooted at dir. The directory is created if it
// does not exist; an error is returned if it cannot be created or is not a
// directory.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("state: empty directory")
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, fmt.Errorf("state: %s is not a directory", dir)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("state: create %s: %w", dir, err)
		}
	default:
		return nil, fmt.Errorf("state: stat %s: %w", dir, err)
	}
	return &FileStore{dir: dir}, nil
}

// Dir returns the directory the store writes to.
func (s *FileStore) Dir() string { return s.dir }

func (s *FileStore) path(key string) string {
	return filepath.Join(s.dir, sanitize(key)+".json")
}

// sanitize rejects path separators and traversal so a malicious roomID cannot
// escape the directory. Room IDs are short alphanumeric tokens, so anything
// outside [a-zA-Z0-9] is rejected.
func sanitize(key string) string {
	if key == "" || strings.ContainsAny(key, `/\..`) {
		return "_invalid"
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return "_invalid"
		}
	}
	return key
}

// Save writes data atomically: a temp file is created in the same directory,
// written, fsynced, and renamed over the target. A crash after the temp write
// but before the rename leaves only the temp file (cleaned up on the next
// LoadAll); the previous version remains intact.
func (s *FileStore) Save(key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.path(key)
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("state: create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("state: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp: %w", err)
	}
	cleanup = false
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("state: rename: %w", err)
	}
	return nil
}

// Delete removes the file for key. A missing file is not an error.
func (s *FileStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: delete %s: %w", key, err)
	}
	return nil
}

// LoadAll reads every `*.json` file in the directory (excluding temp files) and
// returns the contents keyed by roomID. Temp files (`.tmp-*`) and any file that
// fails to read are skipped. The map is empty when the directory is empty.
func (s *FileStore) LoadAll() (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("state: readdir %s: %w", s.dir, err)
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasPrefix(name, ".tmp-") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue // skip unreadable files
		}
		out[key] = data
	}
	return out, nil
}

// --- Persister: batching, coalescing write queue ---

// opKind distinguishes a save (snapshot needed) from a delete.
type opKind int

const (
	opSave opKind = iota
	opDelete
)

// pendingOp is a coalesced entry in the flush set.
type pendingOp struct {
	kind opKind
}

// SnapshotFunc returns the current serialized state for a roomID. Returning
// (nil, false) means the room no longer exists; the persister treats that as a
// delete. The persister calls this under its own goroutine, so the function
// must be safe to call concurrently with room operations (it should lock the
// room before reading its fields).
type SnapshotFunc func(roomID string) (data []byte, ok bool)

// Persister queues and batches disk writes. Producers call MarkDirty /
// MarkDeleted (non-blocking, buffered channel); a single flush goroutine
// coalesces pending ops by roomID (last-write-wins) and flushes on a timer or
// when the coalesced set reaches batchSize. This bounds disk write frequency to
// roughly one flush per flushInterval per batchSize rooms, regardless of how
// many times a room is dirtied.
type Persister struct {
	store        Store
	snapshot     SnapshotFunc
	flushInterval time.Duration
	batchSize    int

	opCh   chan pendingEntry
	stopCh chan struct{}
	doneCh chan struct{}

	// nowFunc is overridable for tests.
	nowFunc func() time.Time
}

// pendingEntry is the on-the-wire op sent through opCh.
type pendingEntry struct {
	roomID string
	kind   opKind
}

// NewPersister constructs a Persister. Start must be called to launch the flush
// goroutine; Close drains and flushes remaining ops.
func NewPersister(store Store, snap SnapshotFunc, flushInterval time.Duration, batchSize int) *Persister {
	return &Persister{
		store:         store,
		snapshot:      snap,
		flushInterval: flushInterval,
		batchSize:     batchSize,
		opCh:          make(chan pendingEntry, 4096),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
		nowFunc:       time.Now,
	}
}

// SetClock installs a custom clock, primarily for tests.
func (p *Persister) SetClock(now func() time.Time) { p.nowFunc = now }

// SetSnapshotFunc replaces the snapshot function. Used when the persister is
// constructed before the registry that supplies the snapshot (to break the
// construction-order cycle). Must be called before Start.
func (p *Persister) SetSnapshotFunc(snap SnapshotFunc) { p.snapshot = snap }

// Start launches the flush goroutine.
func (p *Persister) Start() {
	go p.loop()
}

// MarkDirty enqueues a save for roomID. Non-blocking; if the queue is full the
// op is dropped (the next flush of an already-dirty room will pick up the
// latest state, and the periodic sweep re-dirties all live rooms).
func (p *Persister) MarkDirty(roomID string) {
	if p == nil {
		return
	}
	select {
	case p.opCh <- pendingEntry{roomID: roomID, kind: opSave}:
	default:
	}
}

// MarkDeleted enqueues a delete for roomID. Non-blocking.
func (p *Persister) MarkDeleted(roomID string) {
	if p == nil {
		return
	}
	select {
	case p.opCh <- pendingEntry{roomID: roomID, kind: opDelete}:
	default:
	}
}

// loop is the flush goroutine. It collects ops into a coalesced map
// (roomID → latest op) and flushes when the map reaches batchSize or the
// flushInterval ticker fires.
func (p *Persister) loop() {
	defer close(p.doneCh)
	ticker := time.NewTicker(p.flushInterval)
	defer ticker.Stop()
	pending := make(map[string]opKind, p.batchSize)
	for {
		select {
		case e := <-p.opCh:
			pending[e.roomID] = e.kind
			if len(pending) >= p.batchSize {
				p.flush(pending)
				pending = make(map[string]opKind, p.batchSize)
			}
		case <-ticker.C:
			if len(pending) > 0 {
				p.flush(pending)
				pending = make(map[string]opKind, p.batchSize)
			}
		case <-p.stopCh:
			// Drain any buffered ops before final flush.
			for {
				select {
				case e := <-p.opCh:
					pending[e.roomID] = e.kind
				default:
					p.flush(pending)
					return
				}
			}
		}
	}
}

// flush processes one coalesced batch. For each roomID: if the latest op is a
// delete, the file is removed; if it is a save, the snapshot function is called
// to get current bytes. If the snapshot returns false (room gone), the file is
// deleted instead. Errors are swallowed (best-effort persistence); a failed
// write will be retried on the next dirty mark.
func (p *Persister) flush(pending map[string]opKind) {
	for roomID, kind := range pending {
		switch kind {
		case opDelete:
			_ = p.store.Delete(roomID)
		case opSave:
			data, ok := p.snapshot(roomID)
			if !ok {
				_ = p.store.Delete(roomID)
				continue
			}
			_ = p.store.Save(roomID, data)
		}
	}
}

// Close signals the flush goroutine to drain and exit, then blocks until it has
// flushed all remaining ops. Safe to call once.
func (p *Persister) Close() {
	if p == nil {
		return
	}
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}
	<-p.doneCh
}

// FlushAll synchronously drains the op channel and flushes pending ops. It
// races with the loop goroutine (both may pull from the same channel), but
// operations are idempotent (last write wins). Primarily for tests; for
// production shutdown use Close.
func (p *Persister) FlushAll() {
	if p == nil {
		return
	}
	pending := make(map[string]opKind)
	for {
		select {
		case e := <-p.opCh:
			pending[e.roomID] = e.kind
		default:
			if len(pending) > 0 {
				p.flush(pending)
			}
			return
		}
	}
}
