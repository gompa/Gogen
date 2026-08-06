package server

// Reproduction probe for the reported symptom: switching panes in the
// sidebar session list while a turn is running on the ORIGINAL session
// must not stop that turn. The client's pane switch is a plain
// session_attach of the target pane (focusPane) — the old pane is NOT
// detached and its turn keeps streaming to the connection (D5). This test
// drives the exact message sequence the client sends when the user clicks
// rows in the sidebar session list: A's turn is running, attach B, attach A
// again, attach B again — A's turn must still complete with its own
// turn_end (not cancelled) and its stream events must keep arriving.

import (
	"testing"
	"time"
)

func TestOpenPanesSwitchKeepsOriginalRunningSession(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// Turn in A, kept blocked (turnMu held — a genuinely running turn).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "turn A", SessionID: sidA}); err != nil {
		t.Fatalf("send turn A: %v", err)
	}
	stub.waitBlocked(1)

	// Open pane B (the sidebar "New" button): the pane switches to B
	// server-side; A stays attached in the background.
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
	if sidB == "" {
		t.Fatal("session_new did not produce a new session id")
	}
	// Drain B's attach payload.
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}

	// The user clicks A's row in the open panes panel: session_attach(A)
	// while A's turn is still blocked.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	stateA := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == sidA
	})
	if !stateA.TurnActive {
		t.Fatal("attach A must report turnActive=true (the turn is running)")
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == sidA
	})

	// The user clicks B's row: session_attach(B) while A's turn is STILL
	// running. This is the reported switch — it must not stop A's turn.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
		t.Fatalf("attach B: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == sidB
	})

	// Release A's turn. Its stream/turn_end must still reach the connection
	// (background pane keeps its events, D5) — and it must be a normal
	// turn_end, NOT a cancelled turn.
	stub.releaseN(1)
	terminal := readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.SessionID == sidA && (m.Type == "turn_end" || m.Type == "cancelled")
	})
	if terminal.Type == "cancelled" {
		t.Fatal("switching to another pane CANCELLED the original session's running turn")
	}
}
