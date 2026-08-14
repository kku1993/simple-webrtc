package state

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileStoreSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Save two rooms.
	if err := store.Save("ta0001", []byte(`{"v":1,"roomId":"ta0001"}`)); err != nil {
		t.Fatalf("Save ta0001: %v", err)
	}
	if err := store.Save("ta0002", []byte(`{"v":1,"roomId":"ta0002"}`)); err != nil {
		t.Fatalf("Save ta0002: %v", err)
	}

	all, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("LoadAll = %d entries, want 2", len(all))
	}
	if string(all["ta0001"]) != `{"v":1,"roomId":"ta0001"}` {
		t.Errorf("ta0001 = %s", all["ta0001"])
	}

	// Delete one.
	if err := store.Delete("ta0001"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, _ = store.LoadAll()
	if _, ok := all["ta0001"]; ok {
		t.Errorf("ta0001 should be deleted")
	}
	if len(all) != 1 {
		t.Errorf("LoadAll = %d, want 1", len(all))
	}

	// Delete missing is not an error.
	if err := store.Delete("nonexistent"); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestFileStoreAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	// Write initial.
	_ = store.Save("ta0003", []byte("first"))

	// Overwrite; the file should contain the new content, not a mix.
	_ = store.Save("ta0003", []byte("second"))
	all, _ := store.LoadAll()
	if string(all["ta0003"]) != "second" {
		t.Errorf("after overwrite = %q, want %q", all["ta0003"], "second")
	}

	// No temp files should remain.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if len(name) > 0 && name[0] == '.' {
			t.Errorf("temp file left behind: %s", name)
		}
	}
}

func TestFileStoreCreateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore nested: %v", err)
	}
	if store.Dir() != dir {
		t.Errorf("Dir = %q, want %q", store.Dir(), dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("directory not created")
	}
}

func TestFileStoreRejectsNonDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	_, err := NewFileStore(f)
	if err == nil {
		t.Fatal("expected error for non-directory")
	}
}

func TestFileStoreSanitizesKey(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	// A path-traversal key should be sanitized to _invalid, not escape the dir.
	_ = store.Save("../../../etc/passwd", []byte("bad"))
	all, _ := store.LoadAll()
	if _, ok := all["../../../etc/passwd"]; ok {
		t.Errorf("traversal key should not be stored verbatim")
	}
	// The file should be _invalid.json inside dir, not outside.
	if _, err := os.Stat(filepath.Join(dir, "_invalid.json")); err != nil {
		t.Errorf("sanitized file not found: %v", err)
	}
}

// --- Persister tests ---

func TestPersisterBatchingAndCoalescing(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	var mu sync.Mutex
	snapshots := make(map[string][]byte)
	snapshotFunc := func(roomID string) ([]byte, bool) {
		mu.Lock()
		defer mu.Unlock()
		data, ok := snapshots[roomID]
		return data, ok
	}

	var saveCount atomic.Int64
	wrappedStore := &countingStore{inner: store, saves: &saveCount}

	p := NewPersister(wrappedStore, snapshotFunc, 20*time.Millisecond, 256)
	p.Start()
	defer p.Close()

	// Mark the same room dirty 100 times rapidly; the persister should
	// coalesce these into a single write per flush.
	mu.Lock()
	snapshots["ta0100"] = []byte(`{"v":1}`)
	mu.Unlock()
	for i := 0; i < 100; i++ {
		p.MarkDirty("ta0100")
	}

	// Wait for at least one flush.
	time.Sleep(100 * time.Millisecond)

	count := saveCount.Load()
	if count > 5 {
		t.Errorf("expected coalesced writes (<=5), got %d", count)
	}

	// Verify the file was written.
	all, _ := store.LoadAll()
	if _, ok := all["ta0100"]; !ok {
		t.Errorf("room ta0100 not persisted after flush")
	}
}

func TestPersisterDelete(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	_ = store.Save("ta0200", []byte(`{"v":1}`))

	snapshotFunc := func(string) ([]byte, bool) { return nil, false }
	p := NewPersister(store, snapshotFunc, 20*time.Millisecond, 256)
	p.Start()
	defer p.Close()

	p.MarkDeleted("ta0200")
	time.Sleep(100 * time.Millisecond)

	all, _ := store.LoadAll()
	if _, ok := all["ta0200"]; ok {
		t.Errorf("room ta0200 should have been deleted")
	}
}

func TestPersisterSnapshotFalseDeletes(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	_ = store.Save("ta0300", []byte(`{"v":1}`))

	// snapshot returns false → persister should delete the file.
	snapshotFunc := func(string) ([]byte, bool) { return nil, false }
	p := NewPersister(store, snapshotFunc, 20*time.Millisecond, 256)
	p.Start()
	defer p.Close()

	p.MarkDirty("ta0300")
	time.Sleep(100 * time.Millisecond)

	all, _ := store.LoadAll()
	if _, ok := all["ta0300"]; ok {
		t.Errorf("room ta0300 should be deleted when snapshot returns false")
	}
}

func TestPersisterBatchSizeTrigger(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	snapshotFunc := func(roomID string) ([]byte, bool) {
		return []byte(`{"v":1,"roomId":"` + roomID + `"}`), true
	}
	// Large flush interval so only batch-size triggers a flush.
	p := NewPersister(store, snapshotFunc, 10*time.Second, 3)
	p.Start()
	defer p.Close()

	p.MarkDirty("ta0401")
	p.MarkDirty("ta0402")
	// Two dirty marks — not enough to trigger batch flush.
	time.Sleep(50 * time.Millisecond)
	all, _ := store.LoadAll()
	if len(all) != 0 {
		t.Errorf("expected no writes before batch size, got %d", len(all))
	}

	// Third dirty mark triggers the batch flush.
	p.MarkDirty("ta0403")
	time.Sleep(50 * time.Millisecond)
	all, _ = store.LoadAll()
	if len(all) != 3 {
		t.Errorf("expected 3 writes after batch trigger, got %d", len(all))
	}
}

func TestPersisterCloseFlushes(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	snapshotFunc := func(roomID string) ([]byte, bool) {
		return []byte(`{"v":1}`), true
	}
	// Long interval so Close must do the flush.
	p := NewPersister(store, snapshotFunc, 10*time.Second, 256)
	p.Start()

	p.MarkDirty("ta0500")
	p.Close()

	all, _ := store.LoadAll()
	if _, ok := all["ta0500"]; !ok {
		t.Errorf("Close should flush pending writes")
	}
}

func TestPersisterNilSafe(t *testing.T) {
	var p *Persister
	// All methods should be nil-safe.
	p.MarkDirty("x")
	p.MarkDeleted("x")
	p.FlushAll()
	p.Close()
}

// countingStore wraps a Store and counts Save calls.
type countingStore struct {
	inner Store
	saves *atomic.Int64
}

func (c *countingStore) Save(key string, data []byte) error {
	c.saves.Add(1)
	return c.inner.Save(key, data)
}

func (c *countingStore) Delete(key string) error {
	return c.inner.Delete(key)
}

func (c *countingStore) LoadAll() (map[string][]byte, error) {
	return c.inner.LoadAll()
}
