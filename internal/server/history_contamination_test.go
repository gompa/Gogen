package server

// Reproduction probe for the reported symptom: session history bloats /
// cross-contaminates when switching sessions. Drives real websocket flows:
// message in A, switch to B, message in B, switch back to A, message again,
// then inspect each session's persisted file for messages belonging to the
// other session. Second test: rapid attach/detach cycling with a blocked turn
// in A, asserting every history the server emits is clean.

import (
	"os"
	"testing"
	"time"
)

func TestHistoryContaminationAcrossSessionSwitches(t *testing.T) {
	s, a, store := newLifecycleServer(t)
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

	// New session B; message in B.
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
	// Drain clear_chat + history + config + context.
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}
	sendAndWait(sidB, "q1 in B")

	// Switch back to A; message in A.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
	sendAndWait(sidA, "q2 in A")

	// Switch to B; message in B.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
		t.Fatalf("attach B: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidB })
	sendAndWait(sidB, "q2 in B")

	// The final persist can lag the turn_end message, so poll the store
	// until both files carry their full 4-message conversations; the content
	// assertions below then verify the sessions are clean.
	tryLoadMsgs := func(id string) ([]string, bool) {
		snap, err := store.LoadInWorkingDir(s.ws.WorkingDir, id)
		if err != nil {
			return nil, false // not persisted yet — keep polling
		}
		var out []string
		for _, m := range snap.Messages {
			if m.Role == "user" || m.Role == "assistant" {
				out = append(out, m.Role+":"+m.Content)
			}
		}
		return out, true
	}
	waitFor(t, 5*time.Second, func() bool {
		gotA, okA := tryLoadMsgs(sidA)
		gotB, okB := tryLoadMsgs(sidB)
		return okA && okB && len(gotA) == 4 && len(gotB) == 4
	})

	loadMsgs := func(id string) []string {
		t.Helper()
		snap, err := store.LoadInWorkingDir(s.ws.WorkingDir, id)
		if err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		var out []string
		for _, m := range snap.Messages {
			if m.Role == "user" || m.Role == "assistant" {
				out = append(out, m.Role+":"+m.Content)
			}
		}
		return out
	}

	gotA := loadMsgs(sidA)
	t.Logf("A messages: %v", gotA)
	gotB := loadMsgs(sidB)
	t.Logf("B messages: %v", gotB)

	for _, m := range gotA {
		if m != "user:q1 in A" && m != "user:q2 in A" && m != "assistant:ok" {
			t.Errorf("session A contains foreign message: %q", m)
		}
	}
	for _, m := range gotB {
		if m != "user:q1 in B" && m != "user:q2 in B" && m != "assistant:ok" {
			t.Errorf("session B contains foreign message: %q", m)
		}
	}
	if len(gotA) != 4 {
		t.Errorf("session A has %d messages, want 4 (2 questions + 2 replies): %v", len(gotA), gotA)
	}
	if len(gotB) != 4 {
		t.Errorf("session B has %d messages, want 4 (2 questions + 2 replies): %v", len(gotB), gotB)
	}
}

// TestHistoryCleanAcrossRapidMidTurnSwitches attaches to a session whose turn
// is RUNNING (blocked in the provider), switches away and back repeatedly,
// then completes the turn. Every `history` message the server sends must be
// a subset of the session's own messages — never duplicates of a user
// question, never a foreign session's content.
func TestHistoryCleanAcrossRapidMidTurnSwitches(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// Start a turn in A and keep it blocked (turnMu held, no reply yet).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1 in A", SessionID: sidA}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Create session B; the pane switches to it while A's turn is in flight.
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

	// Give B content so its attach sends a history (the server skips empty
	// histories). B's turn is the stub's call 2; release it right away.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1 in B", SessionID: sidB}); err != nil {
		t.Fatalf("send to B: %v", err)
	}
	stub.waitBlocked(2)
	stub.releaseN(2)
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidB })

	// Rapidly bounce between A (running turn) and B (idle) several times.
	for i := 0; i < 3; i++ {
		if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidA}); err != nil {
			t.Fatalf("attach A: %v", err)
		}
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidA })
		if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sidB}); err != nil {
			t.Fatalf("attach B: %v", err)
		}
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == sidB })
	}

	// Release A's turn; it completes headless-ish (the client is attached to
	// B now, so the events go to a background pane).
	stub.releaseN(1)
	// Wait for the turn to end.
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sidA })
	// The final persist can lag the turn_end message; poll the store until
	// both files carry their completed 2-message conversations, then assert
	// the content below.
	tryLoadMsgs := func(id string) ([]string, bool) {
		snap, err := store.LoadInWorkingDir(dir, id)
		if err != nil {
			return nil, false // not persisted yet — keep polling
		}
		var out []string
		for _, m := range snap.Messages {
			if m.Role == "user" || m.Role == "assistant" {
				out = append(out, m.Role+":"+m.Content)
			}
		}
		return out, true
	}
	waitFor(t, 5*time.Second, func() bool {
		gotA, okA := tryLoadMsgs(sidA)
		gotB, okB := tryLoadMsgs(sidB)
		return okA && okB && len(gotA) == 2 && len(gotB) == 2
	})

	// Final files must be clean: A = [user q1, assistant done], B = [user q1, assistant done].
	loadMsgs := func(id string) []string {
		t.Helper()
		snap, err := store.LoadInWorkingDir(dir, id)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("load %s: %v", id, err)
		}
		var out []string
		for _, m := range snap.Messages {
			if m.Role == "user" || m.Role == "assistant" {
				out = append(out, m.Role+":"+m.Content)
			}
		}
		return out
	}
	gotA := loadMsgs(sidA)
	t.Logf("A messages: %v", gotA)
	if len(gotA) != 2 {
		t.Errorf("session A has %d messages, want 2 (1 question + 1 reply): %v", len(gotA), gotA)
	}
	for _, m := range gotA {
		if m != "user:q1 in A" && m != "assistant:headless-done" && m != "assistant:done" {
			t.Errorf("session A contains unexpected message: %q", m)
		}
	}
	gotB := loadMsgs(sidB)
	t.Logf("B messages: %v", gotB)
	if len(gotB) != 2 {
		t.Errorf("session B has %d messages, want 2 (1 question + 1 reply): %v", len(gotB), gotB)
	}
	for _, m := range gotB {
		if m != "user:q1 in B" && m != "assistant:done" {
			t.Errorf("session B contains unexpected message: %q", m)
		}
	}
}
