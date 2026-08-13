package session

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"os"
)

func nestedSnap(workingDir, id, parent string) agent.SessionSnapshot {
	return agent.SessionSnapshot{
		WorkingDir: workingDir,
		ParentID:   parent,
		Messages:   []llm.Message{{Role: "user", Content: "job " + id}},
	}
}

// TestNestedCascadeDelete verifies deleting a parent removes its nested
// children (D2) while leaving unrelated sessions alone.
func TestNestedCascadeDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(true)
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child-1", nestedSnap(dir, "child-1", "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child-2", nestedSnap(dir, "child-2", "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("other", nestedSnap(dir, "other", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(dir, "parent"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "other" {
		t.Fatalf("after cascade: %+v, want only 'other'", list)
	}
}

// TestNestedCascadeDeleteGrandchild verifies the cascade is recursive:
// deleting a parent removes its nested children AND their nested children
// (subagent_max_depth >= 2), so no orphaned grandchildren linger — they are
// excluded from the flat list, so nothing else would ever delete them.
func TestNestedCascadeDeleteGrandchild(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(true)
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child", nestedSnap(dir, "child", "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("grandchild", nestedSnap(dir, "grandchild", "child")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(dir, "parent"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("after cascade: %+v, want no sessions left", list)
	}
}

// TestNestedChildrenLockedFallbackOrdering verifies the no-index fallback
// orders children most-recently-updated first (the per-parent cap prunes
// from the tail), mirroring the index path — ReadDir's name order would
// prune the wrong (newest) siblings.
func TestNestedChildrenLockedFallbackOrdering(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(true)
	// Sequential saves give strictly increasing Updated stamps.
	for _, id := range []string{"old", "mid", "new"} {
		if err := s.Save(id, nestedSnap(dir, id, "parent")); err != nil {
			t.Fatal(err)
		}
	}
	// Drop the index so the fallback (session-file scan) is exercised.
	if err := os.Remove(s.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	got := s.nestedChildrenLocked(dir, "parent")
	want := []string{"new", "mid", "old"}
	if len(got) != len(want) {
		t.Fatalf("fallback order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback order = %v, want %v", got, want)
		}
	}
}

// TestNestedPerParentCap verifies the transcript cap: at most
// maxNestedPerParent children per parent, oldest pruned at child save time
// (D2), while the just-saved child is always kept.
func TestNestedPerParentCap(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(true)
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	// Save maxNestedPerParent + 2 children (12); each save prunes beyond 10.
	for i := 0; i < maxNestedPerParent+2; i++ {
		id := "child-" + string(rune('a'+i))
		if err := s.Save(id, nestedSnap(dir, id, "parent")); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var children []string
	for _, info := range list {
		if info.ParentID == "parent" {
			children = append(children, info.ID)
		}
	}
	if len(children) != maxNestedPerParent {
		t.Fatalf("children = %v, want %d", children, maxNestedPerParent)
	}
	// The oldest two ('a','b') were pruned; the newest ('k','l') remain.
	for _, gone := range []string{"child-a", "child-b"} {
		for _, c := range children {
			if c == gone {
				t.Fatalf("oldest child %s should have been pruned; remaining: %v", gone, children)
			}
		}
	}
	if _, err := s.LoadInWorkingDir(dir, "parent"); err != nil {
		t.Fatal(err)
	}
}

// TestNestedParentIDRoundTrip verifies ParentID survives save → load.
func TestNestedParentIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(true)
	snap := nestedSnap(dir, "child", "parent")
	if err := s.Save("child", snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadInWorkingDir(dir, "child")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ParentID != "parent" {
		t.Fatalf("loaded ParentID = %q, want parent", loaded.ParentID)
	}
}

// TestNestedExemptFromGlobalPrune verifies the retention prune skips nested
// sessions (they are bounded by the per-parent cap, not the global counts)
// and cascades the children of any top-level session it prunes — a pruned
// parent must not orphan its children.
func TestNestedExemptFromGlobalPrune(t *testing.T) {
	dir := t.TempDir()
	// Budget: 2 top-level sessions, no age retention.
	s := NewStoreWithOptions(true, StoreOptions{MaxCount: 2, MaxAgeDays: -1})
	s.SetAutoPrune(false) // drive the prune explicitly below
	// Save order: parent (oldest top-level), other, then the nested children.
	for _, id := range []string{"parent", "extra", "other", "child-1", "child-2"} {
		parent := ""
		if id == "child-1" || id == "child-2" {
			parent = "parent"
		}
		if err := s.Save(id, nestedSnap(dir, id, parent)); err != nil {
			t.Fatal(err)
		}
	}
	s.Prune(dir)
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Budget 2 keeps the newest top-level sessions ("extra", "other");
	// "parent" (oldest) is pruned — with its children (cascade).
	if len(list) != 2 || list[0].ID != "other" || list[1].ID != "extra" {
		t.Fatalf("after prune: %+v, want 'other' + 'extra' (parent and children pruned)", list)
	}
}

// TestNestedExemptFromGlobalPruneFallback covers the no-index fallback path
// (legacySessionUpdated): nested sessions must be skipped there too, or the
// per-parent cap is not the only bound on children.
func TestNestedExemptFromGlobalPruneFallback(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{MaxCount: 2, MaxAgeDays: -1})
	s.SetAutoPrune(false)
	for _, id := range []string{"parent", "extra", "other", "child-1", "child-2"} {
		parent := ""
		if id == "child-1" || id == "child-2" {
			parent = "parent"
		}
		if err := s.Save(id, nestedSnap(dir, id, parent)); err != nil {
			t.Fatal(err)
		}
	}
	// Drop the index so the legacy file-scan fallback is exercised.
	if err := os.Remove(s.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	s.Prune(dir)
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "other" || list[1].ID != "extra" {
		t.Fatalf("after fallback prune: %+v, want 'other' + 'extra'", list)
	}
}

// TestNestedPruneKeepsRecentChildren verifies the exemption keeps ALL nested
// children of a live parent even when they would exceed the global count.
func TestNestedPruneKeepsRecentChildren(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{MaxCount: 1, MaxAgeDays: -1})
	s.SetAutoPrune(false)
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Save("child-"+string(rune('a'+i)), nestedSnap(dir, "child-"+string(rune('a'+i)), "parent")); err != nil {
			t.Fatal(err)
		}
	}
	// Protect the parent: with budget 1, a non-exempt child would be
	// pruned — the exemption keeps all of them.
	s.Prune(dir, "parent")
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The parent survives (kept); the children are exempt and survive too.
	if len(list) != 4 {
		t.Fatalf("after prune: %+v, want parent + 3 children (all exempt)", list)
	}
}
