package server

// Regression tests for the resolveRuntime fallback fix: session-scoped
// operations (session_detach, cancel) with an id that is no longer registered
// (evicted by the active-cap, or deleted) must be ignored — NOT silently
// routed to the default session. Pre-fix, a pane closed after eviction sent
// session_detach(evictedID) and detached the DEFAULT session from the
// connection (its broadcasts stopped arriving), and a cancel for an evicted
// session killed the DEFAULT session's running turn.

import (
	"testing"
	"time"

	"gogen/internal/agent"
)

func TestDetachUnknownSessionDoesNotDetachDefault(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Detach an id that was never registered. Pre-fix this resolved to the
	// DEFAULT session and detached it from this connection, so the turn's
	// broadcasts below would never arrive.
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send detach: %v", err)
	}

	// A turn in the default session must still reach this connection.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "ping", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == a.SessionID })
}

func TestCancelUnknownSessionDoesNotCancelDefaultTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn in the default session; it blocks inside the provider.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Cancel with a stale id must be a no-op. Pre-fix it resolved to the
	// DEFAULT session and cancelled this very turn.
	if err := conn.WriteJSON(WSMessage{Type: "cancel", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	// A wrongly-routed cancel would stop the running turn; poll for that
	// wrong-behavior signal instead of sleeping a fixed window.
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("session not registered")
	}
	requireNever(t, time.Second, "stale-id cancel stopped the running turn", func() bool {
		active, _ := rt.turnState()
		return !active
	})

	// The turn must still be alive: releasing it produces the normal
	// assistant completion, which is persisted.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == a.SessionID })
	waitFor(t, 5*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "headless-done" {
				return true
			}
		}
		return false
	})
	_ = agent.SessionSnapshot{}
}
