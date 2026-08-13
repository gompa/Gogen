package server

import (
	"context"
	"fmt"
	"log"

	"gogen/internal/agent"
)

// wsHandleBoardStart starts a dedicated agent session for a board ticket
// (board_op action "start"): the ticket is claimed (assignee = the session
// label), a fresh session is seeded with the ticket prompt and registered
// headless — the user's pane and main tab are untouched — and its first
// turn starts immediately. The session id is stored on the ticket
// (AgentSessionID) so the client can offer "Open agent" on a second click.
//
// Unattended-session guards: a delete approval with zero attached clients
// can never be answered (auto-deny only fires on detach), so the runtime
// denies them fail-closed until the user opens the session; a first-turn
// failure is commented on the ticket via the turnErrorHook so a claimed
// ticket never fails silently. Failures surface as board error notices;
// a lost claim race cleans up the fresh session (no orphan empty sessions).
func (s *Server) wsHandleBoardStart(ws *wsConn, pane **sessionRuntime, op *BoardOpRequest) {
	if s.ws.Store == nil {
		writeNoticeError(ws, "board", "Error: session persistence is disabled")
		return
	}
	bm := s.ws.GetBoardManager()
	item, err := bm.Item(op.ID)
	if err != nil {
		writeNoticeError(ws, "board", "Error: "+err.Error())
		return
	}
	if item.Status == "done" {
		writeNoticeError(ws, "board", fmt.Sprintf("Error: board item #%s is done — move it back to start it again", item.ID))
		return
	}
	// The previous agent session for this ticket was deleted: reset the
	// stale link so the ticket can be started fresh.
	if item.AgentSessionID != "" && !s.sessionExists(item.AgentSessionID) {
		if _, err := bm.ResetAgent(item.ID, "user"); err != nil {
			writeNoticeError(ws, "board", "Error: "+err.Error())
			return
		}
		if item, err = bm.Item(item.ID); err != nil {
			writeNoticeError(ws, "board", "Error: "+err.Error())
			return
		}
	}
	// Refuse before creating anything when no model is configured: the
	// fresh session's provider seeds from the workspace default, and a
	// first turn without a model would fail silently (nobody is attached).
	if s.ws.DefaultModel() == "" {
		writeNoticeError(ws, "board", "Error: no model configured — pick a model in Settings first")
		return
	}
	label := ticketSessionLabel(item)
	prompt := agent.TicketPrompt(item, s.ws.GetRuntimeConfig().BoardStartPrompt)
	snap := &agent.SessionSnapshot{
		WorkingDir: s.ws.GetWorkingDir(),
		// Messages deliberately NOT seeded: startTurn passes the prompt as
		// the turn's user message (StreamProcessInput appends it), so the
		// transcript would otherwise carry the prompt twice.
	}
	if old := *pane; old != nil && old.agent != nil {
		// Mirror createNewSession: inherit the pane's thinking level.
		if _, level := old.agent.ModeAndThinkingLevel(); level != "" {
			snap.ThinkingLevel = string(level)
		}
	}
	rt, _ := s.registerSeededSession(snap, label)
	sid := rt.agent.SessionID

	if _, err := bm.Claim(item.ID, label); err != nil {
		// A concurrent start won the claim: remove the fresh session and
		// report the claim error — no orphan empty sessions.
		s.removeFreshSession(sid)
		writeNoticeError(ws, "board", "Error: "+err.Error())
		return
	}
	if _, err := bm.AttachAgent(item.ID, sid, label); err != nil {
		// The claim persisted but the link write failed (disk error): the
		// session is still valid — log and continue; the ticket keeps its
		// assignee and the session is reachable from the sidebar.
		log.Printf("board start: attach agent link on #%s: %v", item.ID, err)
	}

	// Unattended approval guard: with zero attached clients a delete
	// approval can never be answered, so deny it fail-closed (the agent
	// adapts to the denied result). Once the user opens the session, the
	// normal approval flow applies.
	rt.approverOverride = func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if rt.clientCount() == 0 {
			return false, nil
		}
		return rt.deleteApprover()(ctx, req)
	}
	// First-turn failure visibility: comment the error on the ticket.
	ticketID := item.ID
	rt.turnErrorHook = func(err error) {
		if _, cerr := bm.Comment(ticketID, fmt.Sprintf("agent session %s failed: %v", sid, err), "system"); cerr != nil {
			log.Printf("board start: comment turn error on #%s: %v", ticketID, cerr)
		}
	}

	// Acquire the turn BEFORE attaching the tab: a failed acquire (runtime
	// evicted / busy) must not leave the tab attached to a session it was
	// never told about, and the cleanup below has nothing to unwind.
	if !s.acquireTurnForHandler(ws, rt) {
		// The turn could not start (runtime evicted / busy): undo the start
		// so no claimed-but-idle ticket and no orphan empty session survive.
		// No turn has run, so removing the fresh session is safe (same as
		// the lost-claim race path); the ticket goes back to backlog via
		// ResetAgent so the user can retry. The notice below is the only
		// client feedback (UI channel — the board op must never render into
		// the chat transcript).
		s.removeFreshSession(sid)
		if _, err := bm.ResetAgent(item.ID, "user"); err != nil {
			log.Printf("board start: reset ticket #%s after failed start: %v", item.ID, err)
		}
		writeNoticeError(ws, "board", fmt.Sprintf("Error: could not start the agent session for board item #%s; the ticket was reset", item.ID))
		return
	}

	// Background attach: the initiating tab becomes a viewer of the ticket
	// session WITHOUT switching the pane or main tab (the user stays on the
	// board). Delete approvals then reach the tab — the client renders the
	// approval modal globally, even though no pane owns the session — and
	// closing the tab detaches normally, so the standard approval-hold /
	// auto-deny-on-detach machinery applies. The deny-when-unattended
	// override above still covers approvals that arrive after the tab is
	// gone (between detach and turn end).
	rt.attach(ws)
	rt.startTurn(ws, prompt, nil)
	s.broadcastBoardState()
	writeNotice(ws, "board", true, fmt.Sprintf("Started agent session %s for board item #%s: %s", sid, item.ID, item.Title))
}

// ticketSessionLabel derives the started session's sidebar label and board
// assignee from the ticket (same text, so the agent's own board actions use
// the identity the ticket was claimed with).
func ticketSessionLabel(item *agent.BoardItem) string {
	label := fmt.Sprintf("ticket #%s: %s", item.ID, item.Title)
	if len(label) > 60 {
		label = label[:60] + "…"
	}
	return label
}

// sessionExists reports whether a session id is live (registry) or saved
// (store). Used to detect stale ticket→session links.
func (s *Server) sessionExists(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := s.registry.get(id); ok {
		return true
	}
	if s.ws.Store == nil {
		return false
	}
	_, err := s.ws.Store.LoadInWorkingDir(s.ws.GetWorkingDir(), id)
	return err == nil
}

// removeFreshSession evicts a just-created session that must not survive
// (the board start's claim lost a race): delete the file and unregister the
// runtime. No clients are attached and no turn has started.
func (s *Server) removeFreshSession(id string) {
	if s.ws.Store != nil {
		if err := s.ws.Store.Delete(s.ws.GetWorkingDir(), id); err != nil {
			log.Printf("board start: delete fresh session %s: %v", id, err)
		}
	}
	s.registry.remove(id)
}
