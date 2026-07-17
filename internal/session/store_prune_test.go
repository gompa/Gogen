package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

func TestStorePrunesOldSessions(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{MaxCount: 2, MaxAgeDays: 365})
	for i, id := range []string{"a", "b", "c"} {
		snap := agent.SessionSnapshot{
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
	if err := store.Save("c", agent.SessionSnapshot{WorkingDir: dir, Messages: []llm.Message{{Role: "user", Content: "c"}}}); err != nil {
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
