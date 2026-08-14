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

// TestStoreInfo verifies the index-only metadata read used by the web
// delete path to discover a deleted session's parent link: no message
// payload is loaded, and missing sessions return nil.
func TestStoreInfo(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	if info := store.Info(dir, "missing"); info != nil {
		t.Fatalf("Info(missing) = %+v, want nil", info)
	}
	if err := store.Save("parent", agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "p"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("child", agent.SessionSnapshot{
		WorkingDir: dir,
		ParentID:   "parent",
		Messages:   []llm.Message{{Role: "user", Content: "c"}},
	}); err != nil {
		t.Fatal(err)
	}
	info := store.Info(dir, "child")
	if info == nil {
		t.Fatal("Info(child) = nil, want the index entry")
	}
	if info.ParentID != "parent" {
		t.Fatalf("Info(child).ParentID = %q, want parent", info.ParentID)
	}
	if info.MessageCount != 1 || info.ID != "child" {
		t.Fatalf("Info(child) = %+v, want id + message count", info)
	}
	if store.Info(dir, "missing") != nil {
		t.Fatal("Info(missing) must stay nil")
	}
}
