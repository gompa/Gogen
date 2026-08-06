package server

import (
	"testing"
	"time"
)

// TestTwoSessionsStreamConcurrently drives two sessions on ONE connection
// (the Phase 5 shape): a turn starts in the default session, the pane switches
// to a new session (session_new) which starts its own turn, and both streams
// reach the connection tagged with their own sessionId — the background pane's
// events keep flowing because attachment is client-managed (D5).
func TestTwoSessionsStreamConcurrently(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sessionA := a.SessionID

	// Turn 1 in session A.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "turn A", SessionID: sessionA}); err != nil {
		t.Fatalf("send turn A: %v", err)
	}
	stub.waitBlocked(1)

	// Open a second pane/session; the pane switches to it.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	cfgB := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sessionB := cfgB.SessionID
	if sessionB == "" || sessionB == sessionA {
		t.Fatalf("session_new config sessionId = %q, want a fresh session != %q", sessionB, sessionA)
	}

	// Turn 2 in session B.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "turn B", SessionID: sessionB}); err != nil {
		t.Fatalf("send turn B: %v", err)
	}
	stub.waitBlocked(2)

	// Release both turns.
	stub.releaseN(1)
	stub.releaseN(2)

	// Collect stream/turn_end events: both sessions' turns must complete and
	// their events must carry the correct sessionId (session A is a background
	// pane now — the connection stays attached, D5).
	var endA, endB bool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (!endA || !endB) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Logf("read: %v", err)
			continue
		}
		switch msg.Type {
		case "turn_end":
			switch msg.SessionID {
			case sessionA:
				endA = true
			case sessionB:
				endB = true
			default:
				t.Fatalf("turn_end for unknown session %q", msg.SessionID)
			}
		case "user_acked":
			if msg.SessionID != sessionA && msg.SessionID != sessionB {
				t.Fatalf("user_acked for unknown session %q", msg.SessionID)
			}
		}
	}
	if !endA {
		t.Fatal("session A's turn did not complete (background pane lost its events)")
	}
	if !endB {
		t.Fatal("session B's turn did not complete")
	}
}
