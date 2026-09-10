package tui

// Regression test for the signal-exit sweep: flushAndQuit (in-app quit) and
// TUI.Run's post-p.Run sweep share flushAllSessions, which must persist a
// DIRTY background live session (a signal exit otherwise only reached the
// default agent via main's deferred FlushPending). Relies on the agent fix
// that keeps a failed write dirty, so the sweep has something to retry.

import (
	"fmt"
	"sync"
	"testing"

	"gogen/internal/agent"
)

// failOnceAgentStore fails the first write, then records every snapshot.
type failOnceAgentStore struct {
	mu    sync.Mutex
	fails int
	saved []agent.SessionSnapshot
}

func (s *failOnceAgentStore) Save(_ string, snap agent.SessionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return fmt.Errorf("transient save failure")
	}
	s.saved = append(s.saved, snap)
	return nil
}

func (s *failOnceAgentStore) AppendMessages(_ string, snap agent.SessionSnapshot, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return fmt.Errorf("transient delta failure")
	}
	s.saved = append(s.saved, snap)
	return nil
}

func (s *failOnceAgentStore) LoadInWorkingDir(string, string) (agent.SessionSnapshot, error) {
	return agent.SessionSnapshot{}, nil
}
func (s *failOnceAgentStore) List(string) ([]agent.SessionInfo, error) { return nil, nil }
func (s *failOnceAgentStore) LatestID(string) (string, error)          { return "", nil }
func (s *failOnceAgentStore) Delete(string, string) error              { return nil }
func (s *failOnceAgentStore) TouchSession(string, string) error        { return nil }

func (s *failOnceAgentStore) snapshots() []agent.SessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.SessionSnapshot(nil), s.saved...)
}

func TestFlushAllSessionsSweepsDirtyBackgroundSession(t *testing.T) {
	m := newSidebarFullModel(t)

	bg := newSwitchTestAgent(t)
	store := &failOnceAgentStore{fails: 1}
	bg.SessionStore = store
	bg.SessionID = "bg-dirty"
	bg.WorkingDir = m.agent.WorkingDir
	m.lives.Add(bg, "bg")

	// Make the background session DIRTY without a successful write: the
	// rename sets the label and flushes, but the store fails that one write,
	// so (with the agent retry fix) the session stays dirty with unsaved
	// state.
	if _, err := bg.RenameSession("bg-renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := len(store.snapshots()); got != 0 {
		t.Fatalf("setup: failed write persisted %d snapshots, want 0", got)
	}

	// The exit sweep (shared by flushAndQuit and TUI.Run's ctx-cancel path)
	// must flush the dirty background session even though the focused agent
	// is clean.
	m.flushAllSessions()

	snaps := store.snapshots()
	if len(snaps) == 0 {
		t.Fatal("flushAllSessions did not persist the dirty background session")
	}
	if last := snaps[len(snaps)-1]; last.Label != "bg-renamed" {
		t.Fatalf("persisted label = %q, want bg-renamed", last.Label)
	}
}
