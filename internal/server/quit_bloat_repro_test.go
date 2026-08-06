package server

// Reproduction probe for the reported symptom: quitting the server bloats
// the current session's history. Simulates the exact quit path (Phase 6:
// ShutdownSessions + main.go's extra deferred FlushPending) and the restart
// path (restore from store + first turn), asserting the persisted message
// count never grows beyond the in-memory count.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

func TestQuitRestartDoesNotBloatHistory(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	dir := s.ws.WorkingDir

	// Several turns in the current session, so the incremental-delta path
	// and full-save path are both exercised.
	const turns = 6
	for i := 0; i < turns; i++ {
		if _, err := a.StreamProcessInput(context.Background(), fmt.Sprintf("q%d", i), nil); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	id := a.SessionID
	want := a.MessageCount()
	if want != turns*2 {
		t.Fatalf("setup: in-memory messages = %d, want %d", want, turns*2)
	}

	// QUIT: ShutdownSessions (drains + flushes every registered runtime)
	// followed by main.go's outer deferred a.FlushPending().
	s.ShutdownSessions()
	a.FlushPending()

	check := func(step string) {
		t.Helper()
		snap, err := store.LoadInWorkingDir(dir, id)
		if err != nil {
			t.Fatalf("%s: load: %v", step, err)
		}
		if len(snap.Messages) != want {
			t.Fatalf("%s: persisted messages = %d, want %d (history bloated: %v)",
				step, len(snap.Messages), want, msgsContents(snap.Messages))
		}
	}
	check("after quit")

	// RESTART: a fresh agent restored from the store, exactly like main.go's
	// startup restore (RestoreSessionLocal + SessionID = latest).
	snap, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatalf("restart load: %v", err)
	}
	prov := llm.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a2 := agent.NewAgent(prov, exec, ctxMgr)
	a2.SessionStore = store
	a2.SessionID = id
	a2.RestoreSessionLocal(snap, id)
	if got := a2.MessageCount(); got != want {
		t.Fatalf("restart: restored in-memory messages = %d, want %d", got, want)
	}

	// A turn after restart (the first save after restore is a full snapshot
	// that must merge/clear the leftover delta exactly once).
	if _, err := a2.StreamProcessInput(context.Background(), "after restart", nil); err != nil {
		t.Fatalf("post-restart turn: %v", err)
	}
	want2 := a2.MessageCount()
	if want2 != want+2 {
		t.Fatalf("post-restart in-memory messages = %d, want %d", want2, want+2)
	}
	// Quit again.
	a2.FlushSession()
	snap2, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatalf("post-restart load: %v", err)
	}
	if len(snap2.Messages) != want2 {
		t.Fatalf("post-restart quit: persisted messages = %d, want %d (bloated: %v)",
			len(snap2.Messages), want2, msgsContents(snap2.Messages))
	}
}

// TestQuitDoesNotPersistEmptySessions verifies that quitting the server does
// not persist never-used /new panes: ShutdownSessions flushes every
// registered runtime, so before the fix every empty pane was written as a
// 0-message session file + index entry on quit — the sidebar session list
// then bloated with "0 msgs" rows after every restart.
func TestQuitDoesNotPersistEmptySessions(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	dir := s.ws.WorkingDir

	// One turn in the default session so it is worth flushing.
	if _, err := a.StreamProcessInput(context.Background(), "hello", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	usedID := a.SessionID

	// Two /new panes that are never used.
	pane := s.registry.first()
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new 1: %v", err)
	}
	emptyID1 := pane.agent.SessionID
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new 2: %v", err)
	}
	emptyID2 := pane.agent.SessionID
	if emptyID1 == emptyID2 || emptyID1 == usedID {
		t.Fatal("setup: expected three distinct sessions")
	}

	// QUIT: the Phase 6 path plus main.go's outer deferred FlushPending.
	s.ShutdownSessions()
	a.FlushPending()

	// The used session must be on disk.
	snap, err := store.LoadInWorkingDir(dir, usedID)
	if err != nil || len(snap.Messages) == 0 {
		t.Fatalf("used session not flushed: err=%v msgs=%d", err, len(snap.Messages))
	}
	// The never-used panes must NOT appear in the saved list (no file, no
	// index entry): otherwise the sidebar bloats with "0 msgs" sessions.
	list, err := store.List(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for _, ent := range list {
		seen[ent.ID] = true
	}
	for _, id := range []string{emptyID1, emptyID2} {
		if seen[id] {
			t.Errorf("empty session %s was persisted on quit and appears in the saved list", id)
		}
		if _, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", id+".json")); !os.IsNotExist(err) {
			t.Errorf("empty session %s has a file on disk (err=%v)", id, err)
		}
	}
}

func msgsContents(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Role+":"+m.Content)
	}
	return out
}
