package server

import (
	"strings"
	"testing"
	"time"
)

// TestBoardOpViaWS drives the kanban tab's board_op channel: add a card,
// receive the board_state broadcast, move it via drag-drop semantics, and
// verify the disabled-feature gate rejects ops.
func TestBoardOpViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	// Feature off: ops are rejected on the notice channel (never "response",
	// which renders into the chat transcript).
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "list"}}); err != nil {
		t.Fatalf("send board_op list: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "board" || resp.Success || !strings.Contains(resp.Content, "disabled") {
		t.Fatalf("disabled board notice = %+v, want board error", resp)
	}

	// Enable the feature via the config WS, then operate.
	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "on", SessionID: sid}); err != nil {
		t.Fatalf("send board on: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.Board == "on" })

	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "add", Title: "UI card", Description: "from the kanban tab", Priority: "high"}}); err != nil {
		t.Fatalf("send board_op add: %v", err)
	}
	// Successful add: the server broadcasts the fresh board_state, then
	// toasts a success notice to the initiator.
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !ack.Success || ack.Kind != "board" || !strings.Contains(ack.Content, "Added board item #1") {
		t.Fatalf("add ack notice = %+v", ack)
	}
	if state.BoardState == nil || len(state.BoardState.Items) != 1 {
		t.Fatalf("board_state items = %+v, want 1", state.BoardState)
	}
	card := state.BoardState.Items[0]
	if card.ID != "1" || card.Title != "UI card" || card.Status != "backlog" {
		t.Fatalf("unexpected card: %+v", card)
	}

	// Move (drag-drop semantics): broadcast + success notice.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "move", ID: "1", Column: "in_progress"}}); err != nil {
		t.Fatalf("send board_op move: %v", err)
	}
	state = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !ack.Success || !strings.Contains(ack.Content, "Moved board item #1") {
		t.Fatalf("move ack notice = %+v", ack)
	}
	if state.BoardState.Items[0].Status != "in_progress" {
		t.Fatalf("card status = %q, want in_progress", state.BoardState.Items[0].Status)
	}

	// Agent board-tool mutations also broadcast: NewServer wired the
	// on-board-changed hook to broadcastBoardState + broadcastBoardNotice.
	// (The agent-side firing of the hook is covered in the agent package
	// test suite; here we verify the server wiring.)
	if s.ws.BoardChangedHook != nil {
		s.ws.BoardChangedHook("Moved board item #1 to in_progress: UI card")
	}
	state = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	agentAck := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !agentAck.Success || agentAck.Kind != "board" || !strings.Contains(agentAck.Content, "Moved board item #1") {
		t.Fatalf("agent-triggered notice = %+v", agentAck)
	}
	if state.BoardState == nil || len(state.BoardState.Items) != 1 || state.BoardState.Items[0].Status != "in_progress" {
		t.Fatalf("expected board_state broadcast via hook, got %+v", state.BoardState)
	}

	// Invalid op is rejected with a board error notice, no broadcast.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "explode"}}); err != nil {
		t.Fatalf("send invalid op: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "board" || resp.Success || !strings.Contains(resp.Content, "unknown board op") {
		t.Fatalf("invalid op notice = %+v", resp)
	}

	// Remove: broadcast first, then a success notice (destructive op
	// confirmation); the card is gone.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "remove", ID: "1"}}); err != nil {
		t.Fatalf("send board_op remove: %v", err)
	}
	state = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !ack.Success || ack.Kind != "board" || !strings.Contains(ack.Content, "Removed board item #1") {
		t.Fatalf("remove ack notice = %+v", ack)
	}
	if state.BoardState == nil || len(state.BoardState.Items) != 0 {
		t.Fatalf("board_state after remove = %+v, want 0 items", state.BoardState)
	}
}

// TestBoardUpdateAndBlockViaWS covers the two ops backing the card detail's
// inline actions: update (the edit form's full replace of title/description/
// priority) and block (with a required reason). Both broadcast a fresh
// board_state and toast a success notice like every other mutation; a block
// without a reason is rejected on the notice channel.
func TestBoardUpdateAndBlockViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "on", SessionID: sid}); err != nil {
		t.Fatalf("send board on: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.Board == "on" })

	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "add", Title: "before", Description: "d", Priority: "low"}}); err != nil {
		t.Fatalf("send add: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })

	// Update: full replace of the editable fields (title, description,
	// priority, context).
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "update", ID: "1", Title: "after", Description: "new desc", Priority: "urgent", Context: "run go test ./internal/parser"}}); err != nil {
		t.Fatalf("send update: %v", err)
	}
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !ack.Success || ack.Kind != "board" || !strings.Contains(ack.Content, "Updated board item #1") {
		t.Fatalf("update ack = %+v", ack)
	}
	card := state.BoardState.Items[0]
	if card.Title != "after" || card.Description != "new desc" || card.Priority != "urgent" || card.Context != "run go test ./internal/parser" {
		t.Fatalf("updated card = %+v", card)
	}

	// Block with a reason: the card moves to blocked and the reason lands in
	// the activity log.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "block", ID: "1", Reason: "waiting on upstream"}}); err != nil {
		t.Fatalf("send block: %v", err)
	}
	state = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !ack.Success || !strings.Contains(ack.Content, "Blocked board item #1") {
		t.Fatalf("block ack = %+v", ack)
	}
	blocked := state.BoardState.Items[0]
	if blocked.Status != "blocked" {
		t.Fatalf("card status = %q, want blocked", blocked.Status)
	}
	found := false
	for _, act := range blocked.Activity {
		if strings.Contains(act.Text, "waiting on upstream") {
			found = true
		}
	}
	if !found {
		t.Fatalf("block reason missing from activity: %+v", blocked.Activity)
	}

	// An empty block reason is rejected (no broadcast).
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "block", ID: "1", Reason: "  "}}); err != nil {
		t.Fatalf("send block: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Success || resp.Kind != "board" || !strings.Contains(resp.Content, "block reason is required") {
		t.Fatalf("empty block reason notice = %+v", resp)
	}
}
