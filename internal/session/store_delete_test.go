package session

import (
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

func TestDeleteSession(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	id := "sess-del"
	if err := store.Save(id, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "x"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(dir, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", id+".json")); !os.IsNotExist(err) {
		t.Fatalf("expected missing file, err=%v", err)
	}
}

func TestDeleteSessionRejectsPathTraversal(t *testing.T) {
	store := NewStore(true)
	if err := store.Delete("/tmp", "../evil"); err == nil {
		t.Fatal("expected invalid id error")
	}
}
