package server

import (
	"context"
	"fmt"
	"log"
	"strings"

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
//
// The op may carry per-ticket start options: Model ("" = workspace default)
// and Prompt ("" = the configured board_start_prompt template; the ticket's
// stored override is cleared). The model is selected BEFORE the claim so a
// selection failure unwinds with just the fresh-session cleanup; the
// options are persisted on the ticket after the claim so the start popover
// pre-fills on the next start.
func (s *Server) wsHandleBoardStart(ctx context.Context, ws *wsConn, pane **sessionRuntime, op *BoardOpRequest) {
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
	// Refuse before creating anything when neither the op nor the
	// workspace provides a model: the fresh session's provider seeds from
	// the workspace default, and a first turn without a model would fail
	// silently (nobody is attached). An explicit per-ticket model skips
	// the default entirely.
	if op.Model == "" && s.ws.DefaultModel() == "" {
		writeNoticeError(ws, "board", "Error: no model configured — pick a model in the start dialog or set one in Settings first")
		return
	}
	label := ticketSessionLabel(item)
	prompt := agent.TicketPrompt(item, s.ws.GetRuntimeConfig().BoardStartPrompt)
	// Per-ticket prompt template from the start popover wins over the
	// configured template (TicketPrompt substitutes the placeholders).
	if strings.TrimSpace(op.Prompt) != "" {
		prompt = agent.TicketPrompt(item, op.Prompt)
	}
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

	// Per-ticket model: select BEFORE the claim so a failure unwinds with
	// only the fresh-session cleanup (no claim to reset). Catalog lookups
	// are network I/O — done outside any turn lock, same as set_model.
	if op.Model != "" {
		if err := rt.agent.SelectModel(ctx, op.Model); err != nil {
			s.removeFreshSession(sid)
			writeNoticeError(ws, "board", "Error: "+err.Error())
			return
		}
	}

	// Per-ticket reasoning effort: validated against the FINAL model (after
	// SelectModel above, mirroring the subagent effort cascade) and applied
	// before the claim so a failure unwinds with only the fresh-session
	// cleanup (no claim to reset). Empty keeps the pane-inherited level
	// seeded into the snapshot; "off" is a real state (never send
	// reasoning_effort) that overrides the inheritance.
	if strings.TrimSpace(op.ThinkingLevel) != "" {
		level := string(agent.NormalizeThinkingLevel(op.ThinkingLevel))
		if level == "" || !s.isValidThinkingLevel(rt.agent, op.ThinkingLevel) {
			s.removeFreshSession(sid)
			writeNoticeError(ws, "board",
				fmt.Sprintf("Error: reasoning-effort level %q is not accepted by the selected model", strings.TrimSpace(op.ThinkingLevel)))
			return
		}
		rt.agent.SetThinkingLevel(agent.ThinkingLevel(level))
	}

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
	// Persist the start options (per-ticket model/prompt prefill for the
	// next start). The start op is authoritative: an empty value clears
	// the stored override back to the defaults. Non-fatal — the session
	// already started; the popover just re-falls back to the defaults.
	if err := bm.SetStartOptions(item.ID, op.Model, op.Prompt, op.ThinkingLevel); err != nil {
		log.Printf("board start: persist start options on #%s: %v", item.ID, err)
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
	if !rt.acquireTurnForHandler(ws) {
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

	// Background attach: the initiating tab becomes an APPROVAL RECEIVER of
	// the ticket session WITHOUT viewing it (attachPassive, not attach) and
	// without switching the pane or main tab (the user stays on the board).
	// Delete approvals reach the tab — the client renders the approval
	// modal globally, even though no pane owns the session — and closing
	// the tab detaches normally, so the standard approval-hold /
	// auto-deny-on-detach machinery applies (clientCount includes passive
	// attachments). The passive role keeps the session OUT of the
	// live-session signal (viewerCount): once the turn ends, the orphan
	// eviction releases the runtime, so the sidebar never pins a stale
	// "resume to continue" row for a completed ticket nobody is viewing.
	// The deny-when-unattended override above still covers approvals that
	// arrive after the tab is gone (between detach and turn end).
	rt.attachPassive(ws)
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
