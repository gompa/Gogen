package server

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/llm"
)

// readModelUsedBeforeStreamEnd reads frames until the first stream_end of a
// turn, returning the model carried by a model_used frame seen BEFORE it
// ("" when none). This asserts the ordering contract the client relies on:
// model_used must precede the round's stream_end so the live assistant
// bubble is still current when it is stamped.
func readModelUsedBeforeStreamEnd(t *testing.T, conn *websocket.Conn, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var m WSMessage
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type == "model_used" {
			return m.Model
		}
		if m.Type == "stream_end" {
			return ""
		}
	}
	t.Fatal("timed out waiting for stream_end")
	return ""
}

// TestTurnSendsModelUsedBeforeStreamEndAndHistoryCarriesReportedModel
// verifies the provider-reported model is pushed to the web client
// read-only: a
// model_used frame is broadcast BEFORE the round's stream_end (so the client
// can stamp the still-live bubble) and the assistant history entry carries
// the model for replay (router endpoints such as OpenCode Zen resolve
// aliases server-side).
func TestTurnSendsModelUsedBeforeStreamEndAndHistoryCarriesReportedModel(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	stub.model = "glm-4.6"
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "user_acked" })
	stub.releaseN(1) // let the blocking stub's first turn call complete

	if used := readModelUsedBeforeStreamEnd(t, conn, 5*time.Second); used != "glm-4.6" {
		t.Fatalf("model_used before stream_end = %q, want %q", used, "glm-4.6")
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })

	// Attach from a second connection to receive history; the assistant
	// entry must carry the provider-reported model.
	conn2 := dialWS(t, srv, "/ws")
	if err := conn2.WriteJSON(WSMessage{Type: "session_attach", SessionID: a.SessionID}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == a.SessionID
	})
	hist := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == a.SessionID
	})
	found := false
	for _, h := range hist.History {
		if h.Role == "assistant" {
			if h.Model != "glm-4.6" {
				t.Fatalf("history assistant Model = %q, want %q", h.Model, "glm-4.6")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("history contains no assistant entry with a model")
	}
}

// TestTurnWithoutReportedModelLeavesHistoryModelEmpty verifies a provider
// that does not report a model yields no model_used frame and empty history
// model fields, so the default behavior is unchanged.
func TestTurnWithoutReportedModelLeavesHistoryModelEmpty(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub() // model stays empty
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "user_acked" })
	stub.releaseN(1) // let the blocking stub's first turn call complete

	if used := readModelUsedBeforeStreamEnd(t, conn, 5*time.Second); used != "" {
		t.Fatalf("model_used before stream_end = %q, want none", used)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })

	conn2 := dialWS(t, srv, "/ws")
	if err := conn2.WriteJSON(WSMessage{Type: "session_attach", SessionID: a.SessionID}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == a.SessionID
	})
	hist := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == a.SessionID
	})
	for _, h := range hist.History {
		if h.Role == "assistant" && h.Model != "" {
			t.Fatalf("history assistant Model = %q, want empty", h.Model)
		}
	}
}

// TestModelUsedBroadcastForEveryRound verifies model_used is broadcast for
// each round of a multi-round turn — including the intermediate tool-call
// round (the pre-fix gap: only the final round was stamped live, leaving
// intermediate bubbles to be attributed solely by history replay) — and that
// each frame precedes its round's stream_end.
func TestModelUsedBroadcastForEveryRound(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	stub.model = "glm-4.6"
	stub.firstTools = []llm.ToolCall{{
		ID:   "c1",
		Name: "read_file",
		Args: map[string]any{"path": "x"},
	}}
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "read it"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "user_acked" })
	stub.releaseN(1)    // round 1: tool-call round
	stub.waitBlocked(2) // round 2 entered the provider
	stub.releaseN(2)    // round 2: final content round

	// Collect the round-boundary frames in order until the turn completes.
	var frames []WSMessage
	for {
		m := readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
			switch m.Type {
			case "turn_end", "stream_end", "model_used", "thinking":
				return true
			}
			return false
		})
		frames = append(frames, m)
		if m.Type == "turn_end" {
			break
		}
	}

	// Invariant: each round's stream_end must be preceded by its model_used
	// (never after), and every round with a reported model must emit one.
	modelUsed, streamEnds := 0, 0
	awaitingStreamEnd := false
	for _, m := range frames {
		switch m.Type {
		case "model_used":
			if awaitingStreamEnd {
				t.Fatal("duplicate model_used before stream_end")
			}
			if m.Model != "glm-4.6" {
				t.Fatalf("model_used model = %q, want %q", m.Model, "glm-4.6")
			}
			awaitingStreamEnd = true
			modelUsed++
		case "stream_end":
			if !awaitingStreamEnd {
				t.Fatal("stream_end without a preceding model_used")
			}
			awaitingStreamEnd = false
			streamEnds++
		}
	}
	if awaitingStreamEnd {
		t.Fatal("model_used emitted without a following stream_end")
	}
	if modelUsed != 2 {
		t.Fatalf("model_used frames = %d, want 2 (one per round)", modelUsed)
	}
	if streamEnds != 2 {
		t.Fatalf("stream_end frames = %d, want 2 (one per round)", streamEnds)
	}
}
