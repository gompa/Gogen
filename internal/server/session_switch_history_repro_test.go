package server

// Reproduction probe for the reported symptom: "switching between active
// sessions clears previous messages". Drives real websocket flows and asserts
// the CONTENT of every `history` payload the client would render after a
// pane switch — not just the persisted files. A switch must re-derive the
// focused session's FULL transcript (its own messages, complete), never an
// empty or foreign one.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// historyContent returns the user/assistant contents of a history payload.
func historyContent(m WSMessage) []string {
	var out []string
	for _, h := range m.History {
		if h.Role == "user" || h.Role == "assistant" {
			if h.Content != "" {
				out = append(out, h.Role+":"+h.Content)
			}
		}
	}
	return out
}

func wantHistory(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestSwitchBetweenActiveSessionsKeepsHistory is the exact sidebar flow:
// message in A, open B, message in B, switch back to A, switch to B. Every
// attach must deliver that session's complete transcript.
func TestSwitchBetweenActiveSessionsKeepsHistory(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	sendAndWait := func(sid, content string) {
		t.Helper()
		if err := conn.WriteJSON(WSMessage{Type: "message", Content: content, SessionID: sid}); err != nil {
			t.Fatalf("send to %s: %v", sid, err)
		}
		readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "turn_end" && m.SessionID == sid
		})
	}

	// A: one turn.
	sendAndWait(sidA, "q1 in A")

	// Open B (session_new); drain its attach reply.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	var sidB string
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "response" && m.SessionID != "" && m.SessionID != sidA {
			sidB = m.SessionID
			return true
		}
		return false
	})
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}
	sendAndWait(sidB, "q1 in B")

	// Switch back to A: the history the client renders must be A's.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	hA := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	wantHistory(t, historyContent(hA), "user:q1 in A", "assistant:ok")

	// Switch to B: the history must be B's.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
		t.Fatalf("attach B: %v", err)
	}
	hB := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidB })
	wantHistory(t, historyContent(hB), "user:q1 in B", "assistant:ok")
}

// TestSwitchAfterRegistryEvictionKeepsHistory evicts session A from the
// registry (idle orphan eviction) while it is open as a background pane,
// then switches back to it. The pane must re-derive A's FULL transcript from
// the store — the eviction flush must not lose any message.
func TestSwitchAfterRegistryEvictionKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// One completed turn in A.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1 in A", SessionID: sidA}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	stub.releaseN(1)
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidA })

	// Open B; A becomes a background pane.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sidB := cfg.SessionID
	if sidB == sidA {
		t.Fatal("setup: B must differ from A")
	}

	// The client closes pane A (session_detach): with no other clients and no
	// running turn, the runtime is orphan-evicted and flushed.
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: sidA}); err != nil {
		t.Fatalf("detach A: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sidA)
		return !ok
	})

	// The store must still hold A's full transcript (nothing lost by the
	// eviction flush).
	snap, err := store.LoadInWorkingDir(dir, sidA)
	if err != nil {
		t.Fatalf("load A after eviction: %v", err)
	}
	if len(snap.Messages) < 2 {
		t.Fatalf("A persisted %d messages after eviction, want >= 2 (q + reply): %v", len(snap.Messages), snap.Messages)
	}

	// Switch back to A: the attach must re-derive the full transcript.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	hA := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	got := historyContent(hA)
	if len(got) != 2 {
		t.Fatalf("history after re-attach = %v, want [user:q1 in A assistant:ok]", got)
	}
	if got[0] != "user:q1 in A" {
		t.Fatalf("history[0] = %q, want user:q1 in A (transcript lost across eviction: %v)", got[0], got)
	}
}

// TestSwitchMidTurnKeepsOriginalTranscript attaches to a session whose turn is
// RUNNING, then switches away and back. The final attach history must contain
// the ORIGINAL question (never an empty or foreign transcript).
func TestSwitchMidTurnKeepsOriginalTranscript(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// Start a turn in A and keep it blocked.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1 in A", SessionID: sidA}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Open B while A's turn is in flight.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	var sidB string
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "response" && m.SessionID != "" && m.SessionID != sidA {
			sidB = m.SessionID
			return true
		}
		return false
	})
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}

	// Attach A mid-turn: the history must contain the original question.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	hA := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	got := historyContent(hA)
	if len(got) != 1 || got[0] != "user:q1 in A" {
		t.Fatalf("mid-turn history = %v, want [user:q1 in A] (original question lost)", got)
	}

	// Release the turn; it completes while we are attached to A.
	stub.releaseN(1)
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidA })

	// Switching away and back must still show the complete A transcript.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
		t.Fatalf("attach B: %v", err)
	}
	// B is empty (never used): the server sends no history for it. Wait for
	// its config echo instead, then switch back to A.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sidB })
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A again: %v", err)
	}
	hA2 := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	got2 := historyContent(hA2)
	if len(got2) != 2 || got2[0] != "user:q1 in A" {
		t.Fatalf("final A history = %v, want [user:q1 in A assistant:*] (transcript lost)", got2)
	}
	if !strings.HasPrefix(got2[1], "assistant:") {
		t.Fatalf("final A history[1] = %q, want assistant reply", got2[1])
	}
}

// TestSwitchToEmptySavedSessionDoesNotWipeOtherPanes verifies that attaching
// a session with no messages (a /new pane that was never used) does not wipe
// the OTHER open pane's in-memory or on-disk history.
func TestSwitchToEmptySavedSessionDoesNotWipeOtherPanes(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1 in A", SessionID: sidA}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidA })

	// Open B (empty, never used).
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	var sidB string
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if m.Type == "response" && m.SessionID != "" && m.SessionID != sidA {
			sidB = m.SessionID
			return true
		}
		return false
	})
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}

	// Switch to A: full transcript.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	hA := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	wantHistory(t, historyContent(hA), "user:q1 in A", "assistant:ok")

	// Switch to B (empty): the history must be EMPTY, and A must be untouched
	// on disk.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
		t.Fatalf("attach B: %v", err)
	}
	// B is empty: the server sends no history for it (the client renders an
	// empty transcript — correct). Wait for B's config echo instead.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sidB })
	snap, err := store.LoadInWorkingDir(s.ws.WorkingDir, sidA)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	if len(snap.Messages) != 2 {
		t.Fatalf("A persisted %d messages after switching to empty B, want 2", len(snap.Messages))
	}
}

// TestSwitchToSavedSessionWithDeltaHistory re-attaches a session whose
// messages live in BOTH the snapshot and the pending delta file (the
// incremental-persistence shape after an eviction), and verifies the full
// transcript is re-derived — no message may be dropped by the delta merge.
func TestSwitchToSavedSessionWithDeltaHistory(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// Two full turns in A (2 questions + 2 replies).
	for i := 1; i <= 2; i++ {
		if err := conn.WriteJSON(WSMessage{Type: "message", Content: fmt.Sprintf("q%d in A", i), SessionID: sidA}); err != nil {
			t.Fatalf("send: %v", err)
		}
		stub.waitBlocked(i)
		stub.releaseN(i)
		readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidA })
	}

	// Close the pane: the runtime is flushed and evicted. The store now holds
	// A's transcript (snapshot + any pending delta).
	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: sidA}); err != nil {
		t.Fatalf("session_close A: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sidA)
		return !ok
	})
	if _, err := store.LoadInWorkingDir(dir, sidA); err != nil {
		t.Fatalf("load A after close: %v", err)
	}

	// Re-attach A from the store: the FULL transcript must come back.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	hA := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	got := historyContent(hA)
	if len(got) != 4 {
		t.Fatalf("re-attached A history = %v, want 4 messages (2 q + 2 replies)", got)
	}
	if got[0] != "user:q1 in A" || got[2] != "user:q2 in A" {
		t.Fatalf("re-attached A history lost messages: %v", got)
	}
}
