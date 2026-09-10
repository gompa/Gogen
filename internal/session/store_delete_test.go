package session

import (
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/llm"
	"gogen/internal/spill"
)

func TestDeleteSession(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "sess-del"
	if err := store.Save(id, SessionSnapshot{
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

// TestDeleteNeverPersistedSession verifies the missing-file path: a /new
// pane that was never used is not persisted (see Save's empty-session
// skip), yet deleting it is a success — no "session not found" error.
func TestDeleteNeverPersistedSession(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	if err := store.Delete(dir, "never-saved"); err != nil {
		t.Fatalf("Delete of never-persisted session = %v, want nil", err)
	}
}

// TestDeleteDeltaOnlySession verifies deleting a session whose snapshot was
// never written but which has a pending delta (AppendMessages without a
// full Save): the delete succeeds and the delta is cleaned up.
func TestDeleteDeltaOnlySession(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "delta-only"
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "x"}},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(dir, id); err != nil {
		t.Fatalf("Delete of delta-only session = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", id+".delta")); !os.IsNotExist(err) {
		t.Fatalf("expected delta removed, err=%v", err)
	}
}

// TestDeleteRemovesSpillDir verifies the spill lifecycle contract: a
// session's spill directory (.gogen/spill/session-<id>, written by the
// spill package for oversized tool output) is removed with the session,
// while other sessions' spill dirs survive.
func TestDeleteRemovesSpillDir(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id, other := "sess-spill", "sess-keeper"
	for _, sid := range []string{id, other} {
		if err := store.Save(sid, SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: "x"}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := spill.NewStore(dir).Save(sid, "execute_command", []byte("output of "+sid)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(dir, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spill.NewStore(dir).Dir(id)); !os.IsNotExist(err) {
		t.Fatalf("spill dir survived Delete (stat err = %v)", err)
	}
	if _, err := os.Stat(spill.NewStore(dir).Dir(other)); err != nil {
		t.Fatalf("unrelated session's spill dir was removed: %v", err)
	}
}

// TestDeleteRemovesGlobalSpillDir pins the global-mode alignment: sessions
// live in the global data dir there, so their spill trees must too — deleting
// the session removes the global spill dir and never touches the project.
func TestDeleteRemovesGlobalSpillDir(t *testing.T) {
	global := t.TempDir()
	t.Cleanup(func() { spill.SetGlobalRoot("") })
	spill.SetGlobalRoot(global)

	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	store.SetGlobalDir(filepath.Join(global, "sessions"))
	id := "sess-global-spill"
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "x"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := spill.NewStore(dir).Save(id, "execute_command", []byte("output")); err != nil {
		t.Fatal(err)
	}
	spillDir := spill.NewStore(dir).Dir(id)
	if filepath.Dir(spillDir) != global {
		t.Fatalf("spill dir %q is not directly under the global root %q", spillDir, global)
	}
	if err := store.Delete(dir, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spillDir); !os.IsNotExist(err) {
		t.Fatalf("global spill dir survived Delete (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen")); !os.IsNotExist(err) {
		t.Fatalf("global mode wrote into the project dir (stat err = %v)", err)
	}
}

// TestDeleteCascadesSpillToNestedChildren verifies the cascade: deleting a
// parent session deletes its nested (subagent) children's spill dirs too —
// children are never listed or deletable on their own, so leaving their
// spill behind would orphan it permanently.
func TestDeleteCascadesSpillToNestedChildren(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	if err := store.Save("parent", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "p"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("child", SessionSnapshot{
		WorkingDir: dir,
		ParentID:   "parent",
		Messages:   []llm.Message{{Role: "user", Content: "c"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"parent", "child"} {
		if _, err := spill.NewStore(dir).Save(sid, "execute_command", []byte("out")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(dir, "parent"); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"parent", "child"} {
		if _, err := os.Stat(spill.NewStore(dir).Dir(sid)); !os.IsNotExist(err) {
			t.Fatalf("session %s spill dir survived the cascade (stat err = %v)", sid, err)
		}
	}
}

func TestDeleteSessionRejectsPathTraversal(t *testing.T) {
	store := NewStoreWithOptions(true, StoreOptions{})
	if err := store.Delete("/tmp", "../evil"); err == nil {
		t.Fatal("expected invalid id error")
	}
}

// TestStoreInfo verifies the index-only metadata read used by the web
// delete path to discover a deleted session's parent link: no message
// payload is loaded, and missing sessions return nil.
func TestStoreInfo(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	if info := store.Info(dir, "missing"); info != nil {
		t.Fatalf("Info(missing) = %+v, want nil", info)
	}
	if err := store.Save("parent", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "p"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("child", SessionSnapshot{
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
