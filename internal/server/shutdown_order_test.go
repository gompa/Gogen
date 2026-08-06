package server

// Regression test for the sidebar-order shuffle on restart: the shutdown
// sweep (ShutdownSessions) used to call FlushSession on every registered
// runtime, forcing a full write even for sessions whose last turn had
// already flushed. Each write re-stamped the session's Updated timestamp
// with ~now in registry order — the focused session first, so it received
// the OLDEST stamp of the group. On restart, Store.List and Store.LatestID
// order by those timestamps, so the session that was active at shutdown sank
// to the bottom of the saved list and a different session was restored as
// the current pane. The sweep now uses the non-forcing FlushPending, so
// clean sessions keep their real last-activity timestamp and the
// pre-shutdown recency ordering survives.

import (
	"context"
	"testing"
)

func TestShutdownPreservesSessionOrdering(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	dir := s.ws.WorkingDir

	// Session S1 (the default) has an early turn...
	s1ID := a.SessionID
	if _, err := a.StreamProcessInput(context.Background(), "s1 turn", nil); err != nil {
		t.Fatalf("s1 turn: %v", err)
	}

	// ...then a second session S2 is created (the sidebar /new), becomes the
	// focused pane (registry default), and has a LATER turn.
	pane := s.registry.first()
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	s2ID := pane.agent.SessionID
	if s2ID == s1ID {
		t.Fatal("setup: session_new did not create a distinct session")
	}
	if _, err := pane.agent.StreamProcessInput(context.Background(), "s2 turn", nil); err != nil {
		t.Fatalf("s2 turn: %v", err)
	}
	s.registry.setDefault(s2ID) // S2 is the active pane at shutdown

	// Precondition: before quitting, the store's recency order already
	// reflects S2's later activity.
	if latest, err := store.LatestID(dir); err != nil || latest != s2ID {
		t.Fatalf("precondition: LatestID = %s (err=%v), want %s", latest, err, s2ID)
	}

	// QUIT: the Phase 6 sweep plus main.go's outer deferred FlushPending.
	s.ShutdownSessions()
	a.FlushPending()

	// RESTART: the session that was active at shutdown (S2) must still be
	// the most recently updated — it must be restored as current and sit at
	// the top of the saved list. Before the fix, the sweep re-stamped both
	// clean sessions (S2 first, S1 last), so S1 won both checks.
	latest, err := store.LatestID(dir)
	if err != nil {
		t.Fatalf("latest after quit: %v", err)
	}
	if latest != s2ID {
		t.Fatalf("LatestID after quit = %s, want %s (shutdown re-stamped clean sessions and demoted the active one)", latest, s2ID)
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
		t.Fatalf("List[0] after quit = %s, want %s (saved-session order changed on restart)", got, s2ID)
	}
}
