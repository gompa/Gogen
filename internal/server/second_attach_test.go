package server

// Reproduces the live-server observation: attach to a session whose turn is
// RUNNING (turnMu held — the config echo blocks), then attach to a SECOND
// session. The second attach must still deliver session_state + history
// immediately (the first attach's blocked config must not stall it).

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSecondAttachNotBlockedByFirstSessionsRunningTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Session A = the default. Start a turn in it and keep it blocked so
	// turnMu stays held (like the stuck turn on the live server).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold A", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Create session B (fresh) — the pane switches to it server-side.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	sidB := ""
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "response" && m.SessionID != "" && m.SessionID != a.SessionID {
			sidB = m.SessionID
			return true
		}
		return false
	})
	// Drain clear_chat + history.
	for i := 0; i < 2; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}

	// Attach back to A (running turn) — history must arrive, config will lag.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send attach A: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" && m.SessionID == a.SessionID })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == a.SessionID })

	// Now attach to B — must deliver session_state promptly even though A's
	// config echo is still blocked on A's turn lock.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
		t.Fatalf("send attach B: %v", err)
	}
	t1 := time.Now()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" && m.SessionID == sidB })
	t.Logf("attach B: session_state in %v", time.Since(t1))

	// Cleanup: release the turn and wait for its persist before TempDir teardown.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}
