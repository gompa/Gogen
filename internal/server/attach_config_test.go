package server

// The user-visible symptom: switching to a session with a RUNNING turn
// appeared "blocked until the turn finishes" because the attach's config echo
// blocked on turnMu.RLock() — a turn holds turnMu for its entire duration, so
// the session identity/toolbar state (mode, model, label) never arrived until
// the turn ended. The config snapshot is internally synchronized now; it must
// arrive while the turn is still blocked.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAttachConfigNotBlockedByRunningTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn in the default session and keep it blocked: turnMu stays
	// held for the whole turn, exactly like a real running session.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// A fresh connection attaches to the running session. Everything —
	// session_state, history AND config — must arrive while the turn is
	// still blocked (i.e. before releaseN).
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	t0 := time.Now()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" && m.SessionID == a.SessionID })
	hist := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == a.SessionID })
	cfg := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == a.SessionID })
	elapsed := time.Since(t0)
	if len(hist.History) == 0 {
		t.Fatal("history empty (user message missing)")
	}
	if cfg.Mode == "" {
		t.Logf("note: config Mode empty (stub agent); ok")
	}
	// The turn is still blocked — the config must have arrived without
	// waiting for turnMu. Generous bound for scheduling noise; the old
	// behavior was "never until the turn ends".
	if elapsed > 2*time.Second {
		t.Fatalf("config took %v while the turn was running; attach config must not block on turnMu", elapsed)
	}

	// Cleanup: release the turn and let its persist finish before TempDir teardown.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}
