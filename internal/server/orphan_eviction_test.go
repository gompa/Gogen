package server

// Tests for the orphan eviction rule: a session runtime with no attached
// clients and no running turn is unregistered (flush + evict, stays saved on
// disk) — the "resume to continue" state nobody is viewing (a page refresh,
// a closed tab, a re-keyed-away session, or a headless turn that just
// finished) reads as a plain saved session instead. The badge remains only
// for genuinely live sessions: open in another tab (clientCount > 0) or with
// a headless turn still running.

import (
	"testing"
	"time"
)

// TestIdleSessionOrphanEvictedOnTeardown pins the refresh case: closing the
// tab (or refreshing the page) detaches the last client from an idle
// session, and the runtime is unregistered — the next list_sessions would
// report active=false. The conversation stays on disk.
func TestIdleSessionOrphanEvictedOnTeardown(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// Give the session real content so it is saved on disk.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hello", SessionID: sid}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })

	// Close the tab: teardown detaches the last client; the idle runtime is
	// orphan-evicted.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})

	// The conversation is still saved; reopening loads it from the store.
	snap, err := store.LoadInWorkingDir(dir, sid)
	if err != nil {
		t.Fatalf("load orphan-evicted session from store: %v", err)
	}
	if len(snap.Messages) == 0 {
		t.Fatal("orphan-evicted session lost its messages")
	}
}

// TestHeadlessTurnEvictedAfterCompletion pins the busy case: closing the tab
// mid-turn keeps the runtime registered (the turn completes headless — the
// "resume to continue" row is genuine while it runs), and the turn-end hook
// unregisters it once the turn finishes with zero clients.
func TestHeadlessTurnEvictedAfterCompletion(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// Start a turn and leave it blocked in the provider.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "headless", SessionID: sid}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.waitBlocked(1)
	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatal("session not registered")
	}

	// Close the tab mid-turn: busy → the runtime must stay registered.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		return rt.clientCount() == 0
	})
	if active, _ := rt.turnState(); !active {
		t.Fatal("runtime must stay registered while the headless turn runs")
	}

	// The turn completes headless → the turn-end hook evicts the orphan.
	stub.releaseN(1)
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})
	snap, err := store.LoadInWorkingDir(dir, sid)
	if err != nil {
		t.Fatalf("load after headless completion: %v", err)
	}
	if len(snap.Messages) == 0 {
		t.Fatal("headless turn lost its messages on orphan eviction")
	}
}

// TestReKeyedSessionOrphanEvicted pins the typed /new flow: the client sends
// session_detach for the old session after re-keying, and an idle old
// session goes back to the saved list immediately.
func TestReKeyedSessionOrphanEvicted(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// /new creates session B and switches the pane; A stays attached in the
	// background (client-managed, D5).
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	var sidB string
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "response" && m.SessionID != "" && m.SessionID != sidA {
			sidB = m.SessionID
			return true
		}
		return false
	})
	for i := 0; i < 2; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}

	// The client releases A (session_detach on re-key): idle + no clients →
	// orphan-evicted; B stays registered.
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: sidA}); err != nil {
		t.Fatalf("send session_detach: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sidA)
		return !ok
	})
	if _, ok := s.registry.get(sidB); !ok {
		t.Fatal("the current session was evicted by the old session's re-key detach")
	}
}

// TestReconnectForeignDefaultReleasable pins the restart fix: the connect
// handshake attaches the connection to the restored default session even
// when the client's panes are other sessions (a reconnect after a server
// restart whose latest saved session is not the tab's active pane). The
// client releases that attachment (session_detach for the foreign default —
// app.js's onmessage drop branch), and the idle default must then be free
// to orphan-evict: the sessions payload stops reporting it active, so a
// restart does not pin "resume to continue" on the restored (often
// board-started) session.
func TestReconnectForeignDefaultReleasable(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidDefault := a.SessionID

	// Use the default so it is saved (a real restorable session), then
	// re-key to a fresh session — the reconnecting tab's actual pane.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hello", SessionID: sidDefault}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidDefault })

	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	var sidNew string
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "response" && m.SessionID != "" && m.SessionID != sidDefault {
			sidNew = m.SessionID
			return true
		}
		return false
	})

	// The client releases the foreign handshake default (its panes are the
	// fresh session, not the restored one).
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: sidDefault}); err != nil {
		t.Fatalf("send session_detach: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sidDefault)
		return !ok
	})

	// The sessions payload: the released default is listed (saved) but
	// inactive — no "resume to continue" row.
	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	msg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "sessions" && len(m.Sessions) > 0 })
	listed := false
	for _, e := range msg.Sessions {
		if e.ID != sidDefault {
			continue
		}
		listed = true
		if e.Active {
			t.Fatalf("released default session reports active=true (stale 'resume to continue' row)")
		}
	}
	if !listed {
		t.Fatalf("released default session missing from the saved-session list")
	}
	// The pane's session (still viewed by this connection) survives.
	if _, ok := s.registry.get(sidNew); !ok {
		t.Fatal("the pane's session was evicted by the foreign default's release")
	}
}

// TestSessionOpenInAnotherTabNotOrphanEvicted pins the multi-tab guard: the
// last client of a session is only "the last" per session — closing one tab
// must not evict a session another tab still has open.
func TestSessionOpenInAnotherTabNotOrphanEvicted(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	conn2 := dialWS(t, srv, "/ws")
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// conn2 (a second tab viewing the same session) closes: conn1 is still
	// attached, so the runtime must survive. Poll for the wrong-behavior
	// signal (eviction) instead of sleeping a fixed window.
	conn2.Close()
	requireNever(t, time.Second, "runtime was orphan-evicted though another tab is still attached", func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})
}

// TestBootstrapRestoresLatestWhenRegistryEmpty pins the handshake fallback:
// after every runtime is orphan-evicted (or explicitly closed), a new
// connection must still get a session — the most recently saved one — instead
// of "Error: no session available".
func TestBootstrapRestoresLatestWhenRegistryEmpty(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// Use the session so it is saved, then close the only tab: the idle
	// runtime is orphan-evicted and the registry empties.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "hello", SessionID: sid}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })
	conn1.Close()
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})

	// A fresh page connects: the handshake must restore the latest saved
	// session (the one we just closed), not reject the connection.
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	state := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if state.SessionID != sid {
		t.Fatalf("bootstrap session_state sessionId = %q, want the latest saved session %q", state.SessionID, sid)
	}
}

// TestBootstrapCreatesFreshWhenNothingSaved pins the fallback: with no saved
// sessions, an empty registry bootstraps a brand-new session.
func TestBootstrapCreatesFreshWhenNothingSaved(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	oldID := a.SessionID

	// Close the only (never-used, unsaved) tab → orphan eviction empties the
	// registry.
	conn1.Close()
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(oldID)
		return !ok
	})

	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	state := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if state.SessionID == "" || state.SessionID == oldID {
		t.Fatalf("bootstrap sessionId = %q, want a fresh session != %q", state.SessionID, oldID)
	}
}
