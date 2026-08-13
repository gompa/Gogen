package server

import (
	"testing"
	"time"
)

// TestNoticeChannelSeparation verifies the message-type contract in
// ws_types.go: UI-channel operations (board ops, settings toggles,
// working-dir input) never emit "response" messages — those would render
// into the chat transcript and finalize in-flight stream state (the
// mid-stream card-split bug). All their feedback goes out as "notice"
// (or the state channels board_state/config).
func TestNoticeChannelSeparation(t *testing.T) {
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

	ops := []WSMessage{
		// Rejected board op while disabled → error notice.
		{Type: "board_op", BoardOp: &BoardOpRequest{Action: "list"}},
		// Invalid settings toggle → error notice.
		{Type: "config", Board: "maybe", SessionID: sid},
		// Working-dir change in project mode → error notice (global gate).
		{Type: "config", WorkingDir: t.TempDir(), SessionID: sid},
		// Live toggle → config push.
		{Type: "config", Board: "on", SessionID: sid},
		// Board add → board_state only (no ack).
		{Type: "board_op", BoardOp: &BoardOpRequest{Action: "add", Title: "sep"}},
		// Board remove → board_state + success notice.
		{Type: "board_op", BoardOp: &BoardOpRequest{Action: "remove", ID: "1"}},
		// Invalid board op → error notice.
		{Type: "board_op", BoardOp: &BoardOpRequest{Action: "explode"}},
	}
	for _, op := range ops {
		if err := conn.WriteJSON(op); err != nil {
			t.Fatalf("send %s: %v", op.Type, err)
		}
	}
	// Drain a bounded window: every message must be from the allowed
	// non-chat set — never "response". A background reader avoids gorilla's
	// "repeated read on failed websocket connection" panic, which fires
	// after many deadline-expired reads on the same connection.
	type msgCh chan WSMessage
	ch := make(msgCh, 16)
	go func() {
		for {
			var m WSMessage
			if err := conn.ReadJSON(&m); err != nil {
				close(ch)
				return
			}
			ch <- m
		}
	}()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return // connection closed — nothing more to check
			}
			switch m.Type {
			case "notice", "board_state", "config", "session_state",
				"user_term_opened", "user_term_output", "user_term_exit":
			default:
				t.Fatalf("UI-channel op emitted %q (content %q) — must use notice/board_state/config", m.Type, m.Content)
			}
		case <-deadline:
			return
		}
	}
}
