package server

// Tests for the session_close WS message: pressing ✕ on an open pane
// explicitly closes the session — the server cancels any in-flight turn,
// flushes, and unregisters the runtime (when no other socket is attached),
// leaving the session saved on disk and reopenable from the saved list.
// Plain session_detach (typed /new, /resume, /fork re-keys) is untouched:
// the old session keeps running and stays registered.

import (
	"testing"
	"time"
)

// TestSessionCloseIdleUnregistersAndStaysSaved pins the reported symptom:
// after closing an idle session it must NOT keep showing "resume to continue"
// — the runtime is unregistered so the sessions payload reports active=false —
// and the conversation must still be on disk (reopenable from the saved list).
func TestSessionCloseIdleUnregistersAndStaysSaved(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// Give the session real content so closing has something to flush.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hello", SessionID: sid}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })

	// Close the session: ✕ on the open pane.
	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: sid}); err != nil {
		t.Fatalf("send session_close: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})

	// The sessions payload must report active=false (a plain saved row, not
	// "resume to continue").
	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("send list_sessions: %v", err)
	}
	msg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "sessions" })
	var found bool
	for _, se := range msg.Sessions {
		if se.ID == sid {
			found = true
			if se.Active {
				t.Fatal("closed session still reports active=true (sidebar would show 'resume to continue')")
			}
		}
	}
	if !found {
		t.Fatalf("session %s missing from the sessions payload", sid)
	}

	// The conversation is still on disk: reopening loads it from the store.
	snap, err := store.LoadInWorkingDir(dir, sid)
	if err != nil {
		t.Fatalf("load closed session from store: %v", err)
	}
	if len(snap.Messages) == 0 {
		t.Fatal("closed session lost its messages on close")
	}
}

// TestSessionCloseCancelsRunningTurn verifies that closing a session with an
// in-flight turn cancels it (no headless continuation) and unregisters the
// runtime.
func TestSessionCloseCancelsRunningTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// Start a turn and leave it blocked in the provider (turnMu held).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "turn", SessionID: sid}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.waitBlocked(1)
	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatal("session not registered")
	}
	if active, _ := rt.turnState(); !active {
		t.Fatal("turn should be active before close")
	}

	// Close: the turn must be cancelled and the runtime evicted.
	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: sid}); err != nil {
		t.Fatalf("send session_close: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})
	if active, _ := rt.turnState(); active {
		t.Fatal("turn still marked active after session_close")
	}
}

// TestSessionCloseWithOtherClientAttachedDoesNotCancel pins the multi-tab
// guard: closing the pane in one tab must NOT cancel a turn or evict the
// runtime while another tab is still attached to the same session.
func TestSessionCloseWithOtherClientAttachedDoesNotCancel(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// A second tab: its connect handshake attaches to the same default
	// session.
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// conn1 starts a turn, blocked.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "turn", SessionID: sid}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.waitBlocked(1)
	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatal("session not registered")
	}

	// conn2 closes its pane of the same session: conn1 is still attached,
	// so nothing may be cancelled or evicted.
	if err := conn2.WriteJSON(WSMessage{Type: "session_close", SessionID: sid}); err != nil {
		t.Fatalf("send session_close: %v", err)
	}
	// conn2's session_close must not evict the runtime or cancel the turn:
	// poll for either wrong-behavior signal instead of sleeping a fixed
	// window.
	requireNever(t, time.Second, "session_close wrongly evicted the runtime or cancelled the turn (another tab is still attached)", func() bool {
		if _, ok := s.registry.get(sid); !ok {
			return true
		}
		active, _ := rt.turnState()
		return !active
	})

	// The turn still completes for conn1.
	stub.releaseN(1)
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })
}

// TestSessionCloseLastPaneThenNew pins the exact client flow in closePane:
// when the closed pane was the only one, the client follows session_close
// with session_new (a fresh pane). The old session must be evicted and the
// new one must become the connection's usable default.
func TestSessionCloseLastPaneThenNew(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	oldID := a.SessionID

	// Close the only pane while a turn is running, then create a fresh one
	// (the client's closePane → makePane + session_new sequence).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "turn", SessionID: oldID}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.waitBlocked(1)
	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: oldID}); err != nil {
		t.Fatalf("send session_close: %v", err)
	}
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}

	// The new session arrives (session_change reply), and the old runtime is
	// gone from the registry while the new one is the default.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "response" && m.SessionID != "" && m.SessionID != oldID
	})
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(oldID)
		return !ok
	})
	if s.registry.first() == nil || s.registry.first().agent.SessionID == oldID {
		t.Fatal("new session is not the default after close + session_new")
	}
}

// TestSessionCloseUnknownIDIgnored verifies that closing an id that is not
// registered (already evicted/deleted) is a no-op — it must not fall back to
// the default session (resolveRuntime returns nil for unknown ids).
func TestSessionCloseUnknownIDIgnored(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send session_close: %v", err)
	}
	// The default session must be untouched: still registered, still
	// attached (a turn's broadcasts still reach this connection).
	if _, ok := s.registry.get(sid); !ok {
		t.Fatal("default session was evicted by a session_close for an unknown id")
	}
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "ping", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })
}
