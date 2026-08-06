package server

import (
	"context"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

// TestComposerSlashForkWhileHoldingTurnLock is a regression test for the
// composer-typed "/fork" path: handleWSUserMessage holds the pane's turnMu
// (write lock) and then routes "/fork" through runSessionCommand →
// sessionFork. sessionFork used to take src.turnMu.RLock() on the SAME
// mutex from the SAME goroutine — sync.RWMutex is not reentrant, so the
// read lock blocked forever, freezing the connection's read loop and
// leaving the session's turn lock permanently held. The todo snapshot is
// internally synchronized by TodoManager, so the external RLock was both
// redundant and fatal; the fork must complete in bounded time.
func TestComposerSlashForkWhileHoldingTurnLock(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	pane := s.registry.first()
	if pane == nil {
		t.Fatal("no default session")
	}
	// Seed a user+assistant pair so ForkMessages("last") succeeds and the
	// fork reaches the snapshot/session-creation steps.
	pane.agent.RestoreSessionLocal(agent.SessionSnapshot{
		WorkingDir: s.ws.GetWorkingDir(),
		Messages:   []llm.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi there"}},
	}, pane.agent.SessionID)

	// Mirror handleWSUserMessage: acquire the pane's turn lock first, then
	// run the fork on another goroutine (the read loop). If the fork
	// deadlocks on RLock, the goroutine never returns and the test fails
	// with a timeout — and the leaked lock would also wedge the session.
	// Note: runSessionCommand takes &pane and switchPane REBINDS the local
	// pane to the new session, so keep the locked runtime in a separate
	// variable (exactly like handleWSUserMessage's `rt := *pane` capture).
	lockedRT := pane
	lockedRT.turnMu.Lock()
	forked := make(chan error, 1)
	go func() {
		res, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "fork last")
		if err == nil && handled && res.Action == agent.SessionActionClearChat {
			forked <- nil
			return
		}
		forked <- err
	}()
	select {
	case err := <-forked:
		lockedRT.turnMu.Unlock()
		if err != nil {
			t.Fatalf("fork under held turn lock: %v", err)
		}
	case <-time.After(3 * time.Second):
		lockedRT.turnMu.Unlock()
		t.Fatal("fork under held turn lock blocked (RLock self-deadlock)")
	}
	// The lock must be reusable after the fork: a second fork (toolbar path,
	// no lock held) still works and the session can take a turn.
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "fork last"); err != nil {
		t.Fatalf("second fork after unlock: %v", err)
	}
}
