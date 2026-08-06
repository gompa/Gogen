package server

import (
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestNewSessionCreatesAndReKeys drives the client's session_new flow end to
// end (Phase 5): the server must reply with clear_chat + history + config
// carrying the NEW session id, and the new session must be registered and
// become the connection's pane (subsequent messages route to it).
func TestNewSessionCreatesAndReKeys(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sessionA := a.SessionID

	// Sidebar "New" sends session_new (no sessionId).
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	if cfg.SessionID == "" || cfg.SessionID == sessionA {
		t.Fatalf("session_new config sessionId = %q, want a fresh session != %q", cfg.SessionID, sessionA)
	}
	sessionB := cfg.SessionID
	if _, ok := s.registry.get(sessionB); !ok {
		t.Fatal("session_new did not register the new session")
	}

	// A subsequent message routes to the new session (the connection's pane
	// was switched server-side).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hi", SessionID: sessionB}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "user_acked" })
	rt, _ := s.registry.get(sessionB)
	if got := rt.agent.MessageCount(); got != 1 {
		t.Fatalf("new session message count = %d, want 1", got)
	}
}

// TestTypedNewRoutesThroughRegistry drives the typed /new path (D3): a
// message whose content is "/new" must be handled as a registry session
// change, replying with clear_chat + a config carrying the NEW session id.
// This is the client flow that broke when pane routing dropped messages whose
// session id the client had not adopted yet.
func TestTypedNewRoutesThroughRegistry(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sessionA := a.SessionID

	// Typed "/new" arrives as a message tagged with the active pane's session.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "/new", SessionID: sessionA}); err != nil {
		t.Fatalf("send typed /new: %v", err)
	}
	// The response is the first message of the change and must carry the NEW
	// session id plus the clear_chat action (the client relies on this to
	// re-key its pane before history/config arrive).
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if resp.SessionID == sessionA || resp.SessionID == "" {
		t.Fatalf("typed /new response sessionId = %q, want a fresh session != %q", resp.SessionID, sessionA)
	}
	if resp.SessionAction != string(agent.SessionActionClearChat) {
		t.Fatalf("typed /new response sessionAction = %q, want %q", resp.SessionAction, agent.SessionActionClearChat)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	if cfg.SessionID == "" || cfg.SessionID == sessionA {
		t.Fatalf("typed /new config sessionId = %q, want a fresh session != %q", cfg.SessionID, sessionA)
	}
	if _, ok := s.registry.get(cfg.SessionID); !ok {
		t.Fatal("typed /new did not register the new session")
	}
	if s.registry.first().agent.SessionID != cfg.SessionID {
		t.Fatal("typed /new did not make the new session the default")
	}
}

// TestNewSessionInheritsThinkingLevel pins the per-session thinking-level
// contract in the web UI: a /new issued from a pane whose level was changed
// must carry that level into the fresh session instead of resetting to the
// workspace default captured at server startup (ws.ThinkingLevel stays the
// startup "off" here — the old behavior would seed the new session with it).
func TestNewSessionInheritsThinkingLevel(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	if s.ws.ThinkingLevel != string(agent.ThinkingOff) {
		t.Fatalf("workspace default thinking = %q, want %q (startup default)", s.ws.ThinkingLevel, agent.ThinkingOff)
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sidA := cfg.SessionID
	if sidA != a.SessionID {
		t.Fatalf("initial session id = %q, want %q", sidA, a.SessionID)
	}

	// Raise the thinking level on session A.
	if err := conn.WriteJSON(WSMessage{Type: "set_thinking_level", ThinkingLevel: "high", SessionID: sidA}); err != nil {
		t.Fatalf("send set_thinking_level: %v", err)
	}
	thinkCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sidA && m.ThinkingLevel == "high"
	})
	if thinkCfg.ThinkingLevel != "high" {
		t.Fatalf("set_thinking_level config ThinkingLevel = %q, want high", thinkCfg.ThinkingLevel)
	}

	// /new must inherit high rather than resetting to the workspace "off".
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	newCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID != sidA
	})
	if newCfg.ThinkingLevel != "high" {
		t.Fatalf("new session config ThinkingLevel = %q, want high (inherited from previous pane)", newCfg.ThinkingLevel)
	}
	rt, ok := s.registry.get(newCfg.SessionID)
	if !ok {
		t.Fatal("session_new did not register the new session")
	}
	if string(rt.agent.ThinkingLevel) != "high" {
		t.Fatalf("new session agent ThinkingLevel = %q, want high", rt.agent.ThinkingLevel)
	}
}
