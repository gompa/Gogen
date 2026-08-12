package server

// TestAttachNoHistorySkipsHistoryPayload verifies the lightweight
// session_attach used to re-register BACKGROUND panes on reconnect: the
// server must still deliver session_state + config (the pane's busy state /
// toolbar mirrors) but must NOT build or send the full history snapshot
// (plus rewind), which the client would discard anyway. A subsequent full
// attach for the same session must still deliver history.

import (
	"testing"
	"time"
)

func TestAttachNoHistorySkipsHistoryPayload(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	// Drain the connect handshake for the default (empty) session:
	// session_state, then config (+ context). No history yet — no messages.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })

	// Give the session a real conversation so a history snapshot would be
	// non-trivial to build (and worth skipping) if the server sent it.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == a.SessionID })

	// Lightweight re-register: session_state + config must arrive, and the
	// history frame must NOT. The attach goroutine writes history BEFORE
	// config, so once the first config for the session arrives the payload
	// is complete — any history frame would already have been read.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: a.SessionID, NoHistory: true}); err != nil {
		t.Fatalf("send noHistory attach: %v", err)
	}
	seenState, seenConfig := false, false
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "history" && m.SessionID == a.SessionID {
			t.Fatalf("noHistory attach still sent a history payload (%d entries)", len(m.History))
		}
		if m.Type == "session_state" && m.SessionID == a.SessionID {
			seenState = true
		}
		if m.Type == "config" && m.SessionID == a.SessionID {
			seenConfig = true
		}
		return seenState && seenConfig
	})

	// A full attach for the same session must still deliver history: the
	// flag suppresses it only when explicitly requested.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send full attach: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" && m.SessionID == a.SessionID })
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == a.SessionID })
	if len(hist.History) == 0 {
		t.Fatal("full attach history is empty")
	}
}
