package server

// Regression test for the shutdown sweep (ShutdownSessions) with sessions
// that are DIRTY at quit (an in-flight turn). The existing
// TestShutdownPreservesSessionOrdering covers CLEAN sessions — FlushPending
// skips them, so their real last-activity timestamps survive. A dirty
// session still writes at quit, and the sweep used to flush in registry
// order (focused/default session FIRST), so each successive flush re-stamped
// its index Updated with a NEWER ~now — the focused session received the
// OLDEST stamp and was demoted on restart, exactly the bug the clean-session
// fix described. The sweep must flush least-recently-used first so the
// focused (most recently active) session gets the newest stamp.

import (
	"context"
	"testing"
)

// startInFlightTurn runs StreamProcessInput on rt's agent with the runtime's
// stream cancel handles registered (mirroring startTurn), so
// ShutdownSessions' cancelInFlight can cancel and drain it like a real
// headless turn.
func startInFlightTurn(rt *sessionRuntime, input string) {
	streamCtx, streamCancel := context.WithCancel(context.Background())
	errCh := rt.stream.begin(streamCancel)
	go func() {
		defer rt.stream.end()
		defer func() { errCh <- nil }()
		_, _ = rt.agent.StreamProcessInput(streamCtx, input, nil)
	}()
}

func TestShutdownWithInFlightTurnsPreservesOrdering(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)

	// S1 (the default session) has a turn in-flight at shutdown.
	s1ID := a.SessionID
	startInFlightTurn(s.registry.first(), "s1 turn")
	stub.waitBlocked(1)

	// S2 (the focused pane) has a LATER turn in-flight at shutdown.
	pane := s.registry.first()
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	s2ID := pane.agent.SessionID
	if s2ID == s1ID {
		t.Fatal("setup: /new must create a distinct session")
	}
	startInFlightTurn(pane, "s2 turn")
	stub.waitBlocked(2)
	if s.registry.first().agent.SessionID != s2ID {
		t.Fatal("setup: S2 must be the focused session")
	}

	// QUIT: the shutdown sweep. Both sessions are dirty (in-flight turns
	// cancelled by the drain); the focused session (S2) must still be the
	// most recently updated after the sweep. cancelInFlight waits for each
	// turn's deferred flush, so the sweep is synchronous by the time it
	// returns.
	s.ShutdownSessions()

	latest, err := store.LatestID(dir)
	if err != nil {
		t.Fatalf("latest after quit: %v", err)
	}
	if latest != s2ID {
		t.Fatalf("LatestID after quit = %s, want %s (shutdown re-stamped dirty sessions in registry order and demoted the focused one)", latest, s2ID)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatalf("list after quit: %v", err)
	}
	if len(list) == 0 || list[0].ID != s2ID {
		got := "(none)"
		if len(list) > 0 {
			got = list[0].ID
		}
		t.Fatalf("List[0] after quit = %s, want %s (saved-session order wrong after shutdown)", got, s2ID)
	}
}
