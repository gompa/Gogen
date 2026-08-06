package server

// Regression test for the startup/handshake fix: attachSession used to take
// turnMu.RLock() before snapshotting/sending the history. A running turn
// holds turnMu for its ENTIRE duration, so any page open / reconnect while
// the default session was mid-turn got an empty transcript until the turn
// finished (minutes for a long agent run, or indefinitely for a stuck turn).
// The history snapshot is internally locked (agent statsMu) and must never
// wait on the turn lock.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAttachHistoryNotBlockedByRunningTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	// Connection 1 starts a turn and keeps it running (blocking stub).
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold the turn", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1) // turn is now in-flight; turnMu is held

	// Connection 2 is a fresh page open / reconnect while that turn runs.
	// The transcript history must arrive WITHOUT waiting for the turn to end.
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	t0 := time.Now()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	hist := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	elapsed := time.Since(t0)
	if len(hist.History) == 0 {
		t.Fatal("history arrived but is empty (user message should be in it)")
	}
	// The turn is still blocked (stub not released) — if the handshake had
	// waited on the turn lock, history could not have arrived yet. Allow a
	// generous bound for scheduling noise; the previous behavior was "never
	// until release".
	if elapsed > 2*time.Second {
		t.Fatalf("history took %v to arrive while the turn was running; attach must not block on turnMu", elapsed)
	}

	// Clean up: release the turn so teardown is prompt.
	stub.releaseN(1)
	// Wait for the turn to finish AND its deferred persist (FlushSession →
	// store Save) to complete before the test's TempDir cleanup runs;
	// otherwise RemoveAll can race the async write ("directory not empty").
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}
