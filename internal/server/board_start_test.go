package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/llm"
)

// enableBoardAndAddCard turns the board feature on over the WS and adds a
// card ("Fix parser crash"), draining the broadcast + notice.
func enableBoardAndAddCard(t *testing.T, conn *websocket.Conn, sid string) {
	t.Helper()
	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "on", SessionID: sid}); err != nil {
		t.Fatalf("send board on: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.Board == "on" })
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "add", Title: "Fix parser crash", Description: "make go test pass", Priority: "high"}}); err != nil {
		t.Fatalf("send board_op add: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
}

// sendBoardStart sends board_op start for the given card id.
func sendBoardStart(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "start", ID: id}}); err != nil {
		t.Fatalf("send board_op start: %v", err)
	}
}

// TestBoardStartViaWS drives the full ticket-start flow: claim + seeded
// headless session + first turn, with the user's chat pane untouched, and
// the "Open agent" attach afterwards.
func TestBoardStartViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.Model = "default-model"
		p.StreamResults = []*llm.StreamResult{{Content: "ticket done reply"}}
		return p
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	sendBoardStart(t, conn, "1")
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" && m.Kind == "board" })
	if !ack.Success || !strings.Contains(ack.Content, "Started agent session") {
		t.Fatalf("start ack notice = %+v", ack)
	}
	if state.BoardState == nil || len(state.BoardState.Items) != 1 {
		t.Fatalf("board_state = %+v", state.BoardState)
	}
	card := state.BoardState.Items[0]
	if card.Status != "in_progress" || card.Assignee == "" || card.AgentSessionID == "" {
		t.Fatalf("started card = %+v", card)
	}
	agentSid := card.AgentSessionID
	if !strings.HasPrefix(card.Assignee, "ticket #1: ") {
		t.Fatalf("assignee = %q, want the ticket label", card.Assignee)
	}

	// The seeded session exists with the ticket prompt as its first user
	// message, and the headless turn completes (the reply persists).
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), agentSid)
		if err != nil {
			return false
		}
		if len(snap.Messages) < 1 || snap.Messages[0].Role != "user" || !strings.Contains(snap.Messages[0].Content, "Fix parser crash") {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "ticket done reply" {
				return true
			}
		}
		return false
	})

	// "Open agent" after the turn: attach returns the full transcript.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: agentSid}); err != nil {
		t.Fatalf("send attach: %v", err)
	}
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" && m.SessionID == agentSid })
	if len(hist.History) < 2 || !strings.Contains(hist.History[0].Content, "Fix parser crash") {
		t.Fatalf("attach history = %+v", hist.History)
	}
	if hist.History[1].Role != "assistant" || hist.History[1].Content != "ticket done reply" {
		t.Fatalf("attach history reply = %+v", hist.History[1])
	}

	// The start must NOT touch the user's chat pane: no response /
	// clear_chat / history frames (they would re-key or render into the
	// pane). Other frames are allowed — the ticket session is
	// background-attached, so its own stream/turn_end frames reach this
	// tab and are dropped client-side (no pane owns the session).
	// Quiet-window read LAST: gorilla connections are not readable after a
	// timed-out read.
	_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	for {
		var m WSMessage
		if err := conn.ReadJSON(&m); err != nil {
			break // quiet window passed
		}
		switch m.Type {
		case "response", "clear_chat", "history":
			t.Fatalf("board start emitted chat-channel frame %q for the user's pane", m.Type)
		}
	}
}

// TestBoardStartModelAndPrompt verifies the per-ticket start options from
// the "Start agent" popover: the chosen model is selected on the seeded
// session (its persisted snapshot carries it) and the chosen prompt
// template is rendered into the first turn's user message instead of the
// built-in default. Both are persisted on the ticket so the popover
// pre-fills on the next start.
func TestBoardStartModelAndPrompt(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{
			{ID: "default-model", ContextLimit: 128000, Current: true},
			{ID: "m2", ContextLimit: 64000},
		}
		p.StreamResults = []*llm.StreamResult{{Content: "ticket done reply"}}
		return p
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "start", ID: "1", Model: "m2", Prompt: "custom prompt for {title}"}}); err != nil {
		t.Fatalf("send board_op start: %v", err)
	}
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	agentSid := state.BoardState.Items[0].AgentSessionID
	if agentSid == "" {
		t.Fatal("no agent session id")
	}

	// The options are persisted on the ticket (the popover pre-fills from
	// them on the next start).
	card := state.BoardState.Items[0]
	if card.Model != "m2" || card.Prompt != "custom prompt for {title}" {
		t.Fatalf("ticket start options = model %q prompt %q, want m2 / custom prompt for {title}", card.Model, card.Prompt)
	}

	// The session runs with the chosen model and the custom prompt as its
	// first user message — not the built-in default template.
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), agentSid)
		if err != nil {
			return false
		}
		if snap.Model != "m2" {
			return false
		}
		if len(snap.Messages) < 1 || snap.Messages[0].Role != "user" {
			return false
		}
		content := snap.Messages[0].Content
		return strings.Contains(content, "custom prompt for Fix parser crash") &&
			!strings.Contains(content, "You have been assigned board ticket")
	})

	// A start WITHOUT options clears the stored override back to the
	// defaults (the start op is authoritative).
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "start", ID: "1", Model: "", Prompt: ""}}); err != nil {
		t.Fatalf("send board_op start without options: %v", err)
	}
	state = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	card = state.BoardState.Items[0]
	if card.Model != "" || card.Prompt != "" {
		t.Fatalf("start without options left overrides: model %q prompt %q, want empty", card.Model, card.Prompt)
	}
}

// TestBoardStartErrors pins the fast-fail guards: unknown ticket, no model
// configured, done ticket. All reject on the notice channel without
// creating a session.
func TestBoardStartErrors(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider { return llm.NewMockProvider() }
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	startAndExpectError := func(id, want string) {
		t.Helper()
		sendBoardStart(t, conn, id)
		resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
		if resp.Success || resp.Kind != "board" || !strings.Contains(resp.Content, want) {
			t.Fatalf("start(%q) notice = %+v, want error containing %q", id, resp, want)
		}
	}

	// Unknown ticket.
	startAndExpectError("99", "not found")
	// No model configured: refused before creating anything.
	s.ws.SetDefaultModel("")
	startAndExpectError("1", "no model configured")
	// An explicit per-ticket model bypasses the missing default: the
	// popover's choice is selectable on the seeded session.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "start", ID: "1", Model: "mock-model"}}); err != nil {
		t.Fatalf("send board_op start with model: %v", err)
	}
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	ack := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !ack.Success || state.BoardState == nil || len(state.BoardState.Items) == 0 || state.BoardState.Items[0].AgentSessionID == "" {
		t.Fatalf("start with explicit model without default: state=%+v ack=%+v", state, ack)
	}
	// Let the turn's background persistence settle before teardown: the
	// session file must exist with the turn's reply, or the TempDir
	// cleanup races the async save.
	agentSid := state.BoardState.Items[0].AgentSessionID
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), agentSid)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "ok" {
				return true
			}
		}
		return false
	})
	s.ws.SetDefaultModel("mock-model")
	// Done ticket.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "done", ID: "1"}}); err != nil {
		t.Fatalf("send done: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	startAndExpectError("1", "is done")
}

// TestBoardStartApprovalViaBackgroundAttach verifies the background-attach
// contract: the initiating tab is a viewer of the ticket session (no pane
// opened), so the agent's delete approval reaches the tab's modal and the
// user can approve it right from the board.
func TestBoardStartApprovalViaBackgroundAttach(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete",
		Args: map[string]interface{}{"path": "victim.txt"},
	}}
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider { return stub }
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	sendBoardStart(t, conn, "1")
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	agentSid := state.BoardState.Items[0].AgentSessionID
	if agentSid == "" {
		t.Fatal("no agent session id")
	}

	// Round 1 returns the delete tool call; the tool asks for approval,
	// which reaches THIS connection via the background attach (the session
	// has no pane — the user never opened it).
	stub.waitBlocked(1)
	stub.releaseN(1)
	approval := readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "delete_approval" })
	if approval.ApprovalID == "" || approval.SessionID != agentSid {
		t.Fatalf("delete_approval = %+v, want the ticket session's approval", approval)
	}
	// Approve from the board tab: the delete executes, round 2 completes.
	if err := conn.WriteJSON(WSMessage{Type: "delete_approval_response", ApprovalID: approval.ApprovalID, Approved: true, SessionID: agentSid}); err != nil {
		t.Fatalf("send approval response: %v", err)
	}
	stub.waitBlocked(2)
	stub.releaseN(2)
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), agentSid)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "done" {
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("approved delete did not remove the victim file (err=%v)", err)
	}
}

// TestBoardStartApprovalDeniedAfterTabClose verifies the fallback: once the
// initiating tab is gone (detach), a delete approval is denied fail-closed
// (zero attached clients can never answer) and the turn completes instead
// of hanging.
func TestBoardStartApprovalDeniedAfterTabClose(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete",
		Args: map[string]interface{}{"path": "victim.txt"},
	}}
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider { return stub }
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	sendBoardStart(t, conn, "1")
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	agentSid := state.BoardState.Items[0].AgentSessionID
	if agentSid == "" {
		t.Fatal("no agent session id")
	}
	rt, ok := s.registry.get(agentSid)
	if !ok {
		t.Fatal("ticket session not registered")
	}
	stub.waitBlocked(1)

	// The tab closes: the background attach detaches; the turn keeps
	// running headless.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool { return rt.clientCount() == 0 })

	// The delete approval now has zero attached clients → denied
	// fail-closed; round 2 completes the turn — no hang.
	stub.releaseN(1)
	stub.waitBlocked(2)
	stub.releaseN(2)
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), agentSid)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "done" {
				return true
			}
		}
		return false
	})
	// The denied delete never ran: the victim file survives.
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("denied delete removed the victim file: %v", err)
	}
}

// TestBoardStartStaleAgentLink verifies recovery after the started session
// is deleted: the next start resets the stale link and starts fresh.
func TestBoardStartStaleAgentLink(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.Model = "default-model"
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	// First start.
	sendBoardStart(t, conn, "1")
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	sid1 := state.BoardState.Items[0].AgentSessionID
	if sid1 == "" {
		t.Fatal("first start produced no agent session id")
	}

	// Delete the started session (file gone) — the ticket link goes stale.
	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: sid1}); err != nil {
		t.Fatalf("send session_delete: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })

	// Start again: the stale link is reset and a FRESH session is started.
	sendBoardStart(t, conn, "1")
	state = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	sid2 := state.BoardState.Items[0].AgentSessionID
	if sid2 == "" || sid2 == sid1 {
		t.Fatalf("second start agent session = %q, want a fresh id", sid2)
	}
	bm := s.ws.GetBoardManager()
	item, err := bm.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	var sawReset bool
	for _, act := range item.Activity {
		if strings.Contains(act.Text, "no longer exists") {
			sawReset = true
		}
	}
	if !sawReset {
		t.Fatalf("activity missing the reset note: %+v", item.Activity)
	}
}

// TestBoardStartTurnErrorCommentsTicket verifies the turnErrorHook: a
// failed first turn is commented on the ticket instead of failing silently
// in the unattended session.
func TestBoardStartTurnErrorCommentsTicket(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamErr = errors.New("boom")
		return p
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	enableBoardAndAddCard(t, conn, cfg.SessionID)

	sendBoardStart(t, conn, "1")
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })

	bm := s.ws.GetBoardManager()
	waitFor(t, 5*time.Second, func() bool {
		item, err := bm.Item("1")
		if err != nil {
			return false
		}
		for _, act := range item.Activity {
			if strings.Contains(act.Text, "failed: boom") {
				return true
			}
		}
		return false
	})
}
