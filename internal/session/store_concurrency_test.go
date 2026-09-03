package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gogen/internal/llm"
)

// TestStoreConcurrentSaveLoadDelete hammers the store from many goroutines
// with distinct session IDs — the multi-session web server pattern. Before
// the internal mutex, concurrent Save/AppendMessages/Load/Delete raced on
// index.json read-modify-write and the createdCache map. Run with -race.
func TestStoreConcurrentSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	store.SetAutoPrune(false) // registry owns pruning; exercise Save without it

	const n = 12
	const ops = 8
	var wg sync.WaitGroup
	errs := make(chan error, n*ops*2)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sess-%d", i)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			snap := SessionSnapshot{
				WorkingDir: dir,
				Messages:   []llm.Message{{Role: "user", Content: "hello " + id}},
			}
			for j := 0; j < ops; j++ {
				if err := store.Save(id, snap); err != nil {
					errs <- fmt.Errorf("Save %s: %w", id, err)
					return
				}
				if _, err := store.LoadInWorkingDir(dir, id); err != nil {
					errs <- fmt.Errorf("Load %s: %w", id, err)
					return
				}
				if err := store.AppendMessages(id, snap, 1); err != nil {
					errs <- fmt.Errorf("Append %s: %w", id, err)
					return
				}
				if _, err := store.List(dir); err != nil {
					errs <- fmt.Errorf("List: %w", err)
					return
				}
			}
			// TouchSession + LatestID also mutate the index/cache.
			if err := store.TouchSession(dir, id); err != nil {
				errs <- fmt.Errorf("Touch %s: %w", id, err)
				return
			}
			if _, err := store.LatestID(dir); err != nil {
				errs <- fmt.Errorf("LatestID: %w", err)
				return
			}
			if err := store.Delete(dir, id); err != nil {
				errs <- fmt.Errorf("Delete %s: %w", id, err)
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestPruneProtectsMultipleActiveIDs verifies that Prune retains every
// protected ID (all active in-memory sessions, E2) while still dropping
// over-capacity sessions, and that SetAutoPrune(false) suppresses the
// internal Save-time prune.
func TestPruneProtectsMultipleActiveIDs(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{MaxCount: 3, MaxAgeDays: 365})
	store.SetAutoPrune(false)

	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if err := store.Save(id, SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: id}},
		}); err != nil {
			t.Fatal(err)
		}
		// Stagger Updated timestamps so the least-recently-updated sessions
		// are deterministic (a < b < c < d < e).
		time.Sleep(5 * time.Millisecond)
	}

	// Auto-prune is off, so the five saves above must NOT have dropped any
	// session — even though MaxCount is 3.
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if _, err := store.LoadInWorkingDir(dir, id); err != nil {
			t.Fatalf("session %s dropped before explicit Prune (auto-prune should be off): %v", id, err)
		}
	}

	// Protect the two "active" sessions b and d; capacity 3 keeps one more
	// (the most recently updated non-protected session, e).
	store.Prune(dir, "b", "d")

	wantGone := map[string]bool{"a": true, "c": true}
	wantKeep := map[string]bool{"b": true, "d": true, "e": true}
	for id := range wantKeep {
		if _, err := store.LoadInWorkingDir(dir, id); err != nil {
			t.Errorf("protected session %s was pruned: %v", id, err)
		}
	}
	for id := range wantGone {
		path := filepath.Join(dir, ".gogen", "sessions", id+".json")
		if _, err := os.Stat(path); err == nil {
			t.Errorf("over-capacity session %s should have been pruned", id)
		}
	}
}
