package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogen/internal/llm"
	"gogen/internal/spill"
)

func TestStorePrunesOldSessions(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{MaxCount: 2, MaxAgeDays: 365})
	for i, id := range []string{"a", "b", "c"} {
		snap := SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: id}},
		}
		if err := store.Save(id, snap); err != nil {
			t.Fatal(err)
		}
		// Stagger mtimes via UpdatedAt by rewriting file timestamps after save.
		path := filepath.Join(dir, ".gogen", "sessions", id+".json")
		ts := time.Now().Add(time.Duration(i) * time.Second)
		_ = os.Chtimes(path, ts, ts)
		_ = store.Save(id, snap) // refresh UpdatedAt to now; use order via sequential saves
	}
	// Save again with maxCount=2 so prune runs after c.
	if err := store.Save("c", SessionSnapshot{WorkingDir: dir, Messages: []llm.Message{{Role: "user", Content: "c"}}}); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > 2 {
		t.Fatalf("expected at most 2 sessions after prune, got %d", len(list))
	}
}

// TestPruneRemovesSpillDir pins the prune half of the spill lifecycle
// contract (the other half is TestDeleteRemovesSpillDir): a session dropped
// by count/age retention must have its spill dir removed with it, exactly as
// an explicit delete does. Bug: prune inlined its own deletion and only
// removed the delta and archive sidecars, orphaning the pruned session's
// spilled tool output on disk forever.
func TestPruneRemovesSpillDir(t *testing.T) {
	dir := t.TempDir()
	// Capacity 1 with "keep" protected: every OTHER top-level session is
	// over-budget and pruned.
	store := NewStoreWithOptions(true, StoreOptions{MaxCount: 1, MaxAgeDays: 365})
	store.SetAutoPrune(false)

	for _, id := range []string{"old", "keep"} {
		if err := store.Save(id, SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: id}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := spill.NewStore(dir).Save(id, "execute_command", []byte("output of "+id)); err != nil {
			t.Fatal(err)
		}
	}
	// Make "old" the least-recently-updated so retention targets it.
	if err := store.SetUpdatedAt(dir, "old", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	store.Prune(dir, "keep")

	if _, err := os.Stat(spill.NewStore(dir).Dir("old")); !os.IsNotExist(err) {
		t.Fatalf("pruned session's spill dir survived prune (stat err = %v)", err)
	}
	if _, err := os.Stat(spill.NewStore(dir).Dir("keep")); err != nil {
		t.Fatalf("protected session's spill dir was removed: %v", err)
	}
}

// TestPruneOfForkedSourceKeepsChildSpill pins that the prune spill cleanup
// is safe for forked sessions: fork-time repointing hardlinks the source's
// spill files into the CHILD's spill dir (spill.RepointFile), so removing the
// pruned source's dir never strands the child's retrieval — the surviving
// link keeps the inode alive.
func TestPruneOfForkedSourceKeepsChildSpill(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{MaxCount: 1, MaxAgeDays: 365})
	store.SetAutoPrune(false)

	if err := store.Save("src", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "src"}},
	}); err != nil {
		t.Fatal(err)
	}
	srcPath, err := spill.NewStore(dir).Save("src", "execute_command", []byte("shared payload"))
	if err != nil {
		t.Fatal(err)
	}
	// Fork: hardlink the referenced file into the child's spill dir (what
	// RepointSpillLocators does at fork time), and persist the child.
	childPath, err := spill.RepointFile(dir, srcPath, "child")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("child", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "child"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Source becomes the least-recently-updated; the fork child is kept.
	if err := store.SetUpdatedAt(dir, "src", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	store.Prune(dir, "child")

	if _, err := os.Stat(spill.NewStore(dir).Dir("src")); !os.IsNotExist(err) {
		t.Fatalf("pruned source's spill dir survived prune (stat err = %v)", err)
	}
	data, err := os.ReadFile(childPath)
	if err != nil || string(data) != "shared payload" {
		t.Fatalf("child retrieval broke after the source was pruned: %q err %v", data, err)
	}
}
