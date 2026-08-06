package server

// The user-visible symptom: "can't resume my other responding session".
// session_resume of a session with a RUNNING turn had its whole reply
// (response/clear_chat/history/config) blocked on turnMu.RLock() — a turn
// holds turnMu for its entire duration, so the resume appeared to do nothing
// until the turn ended. The reply path is internally synchronized now; it
// must arrive while the turn is still blocked.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResumeRunningSessionReplyNotBlockedByTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn in the default session and keep it blocked: turnMu stays
	// held for the whole turn, exactly like a real responding session.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Resume the same (responding) session. The full reply — response,
	// clear_chat, history, config — must arrive while the turn is STILL
	// blocked (before releaseN).
	t0 := time.Now()
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" && m.SessionID == a.SessionID })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" && m.SessionID == a.SessionID })
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == a.SessionID })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == a.SessionID })
	elapsed := time.Since(t0)
	if resp.Content == "" {
		t.Fatal("resume response empty")
	}
	if len(hist.History) == 0 {
		t.Fatal("resumed history empty (user message missing)")
	}
	// The turn is still blocked — the reply must not have waited for turnMu.
	// Generous bound for scheduling noise; the old behavior was "never until
	// the turn ends".
	if elapsed > 2*time.Second {
		t.Fatalf("resume reply took %v while the turn was running; must not block on turnMu", elapsed)
	}

	// Cleanup: release the turn and let its persist finish before TempDir teardown.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}

// TestResumeRunningSessionSendsSessionState pins the convergence latch: a
// session_resume of a session with a RUNNING turn must send session_state
// (turnActive=true) before the reply, so the client latches the turn-end
// refetch that converges the transcript. The reply's history snapshot cannot
// contain the in-flight reply, and the client only re-attaches on turn_end
// when session_state said the turn was active. Pre-fix, resume sent no
// session_state and the resumed pane stayed missing the in-flight exchange
// until a manual refocus/reconnect.
func TestResumeRunningSessionSendsSessionState(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	// Drain the connect handshake's session_state (default session, idle).
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn in the default session and keep it blocked so the turn is
	// genuinely running (turnMu held) when the resume arrives.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Resume the same (responding) session. The reply must now OPEN with
	// session_state reporting the running turn.
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == a.SessionID
	})
	if !state.TurnActive {
		t.Fatal("resume session_state must report turnActive=true (the turn is running)")
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "response" && m.SessionID == a.SessionID
	})
	if resp.Content == "" {
		t.Fatal("resume response empty")
	}

	// Cleanup: release the turn and let its persist finish before TempDir teardown.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}
