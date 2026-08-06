package server

// Regression tests for save/restore brittleness found while reviewing the
// multi-session persistence paths:
//
//  1. Working-dir changes wrote an incremental DELTA into the NEW directory
//     while the base snapshot lived in the OLD one — the session became
//     unloadable in the new project until the next full save (which never
//     happened if the server quit first).
//  2. Incremental delta saves only carry messages; label/mode/model/
//     thinking/oneshot/profile/todo changes were silently lost if the
//     process quit inside the incremental window (≤5 messages or 30s after
//     the last full snapshot).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/agent"
)

// TestWorkingDirChangeKeepsSessionLoadable: after a working-dir change the
// session must be immediately loadable from the NEW directory with its full
// history — not just from a delta that has no base snapshot there.
func TestWorkingDirChangeKeepsSessionLoadable(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	dir := s.ws.WorkingDir

	// Several turns so the last persist is an incremental delta sitting on
	// top of a RECENT full snapshot (the window that used to break).
	for i := 0; i < 4; i++ {
		if _, err := a.StreamProcessInput(context.Background(), fmt.Sprintf("w%d", i), nil); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	id := a.SessionID
	want := a.MessageCount()

	newDir := filepath.Join(dir, "proj2")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Same sequence the set-dir WS handler runs (under each session's turnMu).
	s.applyWorkingDirToAll(newDir)

	snap, err := store.LoadInWorkingDir(newDir, id)
	if err != nil {
		t.Fatalf("session not loadable in new dir after working-dir change: %v", err)
	}
	if len(snap.Messages) != want {
		t.Fatalf("messages in new dir = %d, want %d", len(snap.Messages), want)
	}
	// A quit right after the change must also persist into the new dir
	// (ShutdownSessions flushes the registered runtime).
	s.ShutdownSessions()
	snap, err = store.LoadInWorkingDir(newDir, id)
	if err != nil {
		t.Fatalf("session not loadable in new dir after shutdown flush: %v", err)
	}
	if len(snap.Messages) != want {
		t.Fatalf("messages in new dir after shutdown = %d, want %d", len(snap.Messages), want)
	}
}

// TestMetadataChangesForceFullSave: label/mode changes made inside the
// incremental-save window (fresh full snapshot, no new messages) must still
// be persisted — quit must not lose them.
func TestMetadataChangesForceFullSave(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	dir := s.ws.WorkingDir
	if _, err := a.StreamProcessInput(context.Background(), "hello", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	id := a.SessionID

	// Rename: the flush runs right after the turn's full save at count=1
	// with 1 new message — the incremental window. Before the fix the new
	// label only went into a delta that loads ignore, so a quit would lose
	// the rename.
	if _, err := a.RenameSession("my-label"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	snap, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Label != "my-label" {
		t.Fatalf("label = %q, want %q (rename lost by incremental save)", snap.Label, "my-label")
	}

	// Mode change is the same class (deltas carry no mode field at all).
	a.SetMode(agent.ModePlan)
	snap, err = store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != "plan" {
		t.Fatalf("mode = %q, want %q (mode lost by incremental save)", snap.Mode, "plan")
	}

	// And a quit must keep both (the double-flush quit path).
	s.ShutdownSessions()
	snap, err = store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Label != "my-label" || snap.Mode != "plan" {
		t.Fatalf("after quit: label = %q mode = %q, want %q/%q",
			snap.Label, snap.Mode, "my-label", "plan")
	}
}

// TestRenamedSessionKeepsTitleInSidebarList pins the reported bug: closing an
// open session loses its title in the sidebar. The sidebar's saved-session
// rows render from the server's sessions payload (store index), while an open
// pane's row shows the live in-memory label — so when a session is renamed
// (RenameSession / the session_rename tool), the two diverged: the index kept
// the first user message and the closed session's row fell back to it. The
// list must carry the rename.
func TestRenamedSessionKeepsTitleInSidebarList(t *testing.T) {
	_, a, _ := newLifecycleServer(t)
	if _, err := a.StreamProcessInput(context.Background(), "implement session commands with labels", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if _, err := a.RenameSession("My Custom Title"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// What the sidebar renders for a SAVED (closed) session: the sessions
	// payload built by the server from the store listing.
	_, sessions, err := a.FormatSessionListForUI()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Label != "My Custom Title" {
		t.Fatalf("sidebar label = %q, want the renamed title (not the first user message)", sessions[0].Label)
	}
}
