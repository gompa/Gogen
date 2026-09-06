package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"gogen/internal/agent"
)

// The auto review agent: when the review-agent feature flag is on, a board
// ticket moved into in_review — by a worker agent's board tool or by the
// user (drag-drop and the move op both land in BoardManager.Move) — spawns
// a headless REVIEW session that verifies the work and either marks the
// ticket done or comments findings and moves it back to in_progress.
//
// The reviewer is a FULL top-level session, not a subagent: a move may be
// user-initiated with no parent session to hang a child off, and the
// reviewer needs its own context window, the board tool, and a sidebar
// row. It shares the board-start machinery (startTicketSession) so the
// model cascade, level validation, and unattended-session guards cannot
// drift from the "Start agent" button.
//
// Guards (in maybeStartReview):
//   - feature flag (settings toggle; default off; TUI never wires the
//     trigger, so review stays manual there),
//   - recursion: a move by the reviewer itself for the same ticket never
//     retriggers (the label prefix identifies it without bookkeeping; the
//     prompt forbids the move anyway — this makes a stray move harmless),
//   - ping-pong cap: MaxReviewRounds started reviews per ticket,
//   - one live reviewer per ticket (ReviewSessionID + registry liveness).
type boardReviewTrigger struct {
	s *Server
	// startMu serializes check-then-start: the hook fires outside the
	// manager lock, so two racing moves would otherwise both pass the
	// "no live reviewer" check and spawn twice.
	startMu sync.Mutex
}

// installReviewAgent wires the trigger onto the workspace's shared board
// manager. Idempotent — called at server construction and again from
// applyBoardManagerToAll when the board feature is toggled on (its manager
// may have just been created). A nil manager (board disabled) is a no-op.
func (s *Server) installReviewAgent() {
	bm := s.ws.GetBoardManager()
	if bm == nil {
		return
	}
	if s.reviewTrigger == nil {
		s.reviewTrigger = &boardReviewTrigger{s: s}
	}
	bm.SetOnReviewNeeded(s.reviewTrigger.maybeStartReview)
}

// maybeStartReview is the BoardManager auto-review hook: fired after a
// successful move into in_review, outside the manager lock. Every guard is
// fail-silent by design — an auto-review that cannot start (flag off,
// capped, already running, no model) must never disrupt the move that
// triggered it; visibility comes from the board notice and, on real
// failures, a comment on the ticket.
func (t *boardReviewTrigger) maybeStartReview(item agent.BoardItem, by string) {
	s := t.s
	if !s.ws.FeatureFlags().ReviewAgentEnabled() {
		return
	}
	bm := s.ws.GetBoardManager()
	if bm == nil {
		return
	}
	t.startMu.Lock()
	defer t.startMu.Unlock()

	// Recursion guard: the review session's own label for this ticket.
	if isReviewActor(item.ID, by) {
		return
	}
	// Ping-pong cap: past MaxReviewRounds started reviews the auto-review
	// stops (the cap is announced once on the ticket so the state is
	// visible without spamming the activity log on every later move).
	if item.ReviewRounds >= agent.MaxReviewRounds {
		t.announceCap(bm, &item)
		return
	}
	// One reviewer per ticket: the linked review session still live in the
	// registry means a review is already running. A stale link (session
	// finished/gone) does not block — the next review starts fresh.
	if item.ReviewSessionID != "" {
		if _, ok := s.registry.get(item.ReviewSessionID); ok {
			return
		}
	}

	// Model cascade: per-ticket override (the card popover's review
	// section) > the configured review_agent_model > the workspace default
	// (the fresh session's provider is seeded with it). An empty cascade
	// with no workspace default is refused: a headless first turn without
	// a model would fail silently (nobody is attached).
	runtimeCfg := s.ws.GetRuntimeConfig()
	model := strings.TrimSpace(item.ReviewModel)
	if model == "" {
		model = strings.TrimSpace(runtimeCfg.ReviewAgentModel)
	}
	if model == "" && s.ws.DefaultModel() == "" {
		log.Printf("review agent: no model configured for board item #%s (set reviewAgentModel or a workspace default)", item.ID)
		t.commentOnce(bm, &item, "review agent did not start: no model configured (set a workspace default model or reviewAgentModel)")
		return
	}
	// Thinking level: per-ticket override > the configured
	// review_agent_thinking_level > "" (inherit the seeded workspace
	// level). Validated against the reviewer's FINAL model by
	// startTicketSession, mirroring the worker start.
	thinking := item.ReviewThinkingLevel
	if strings.TrimSpace(thinking) == "" {
		thinking = runtimeCfg.ReviewAgentThinkingLevel
	}

	prompt := agent.ReviewPrompt(&item, runtimeCfg.BoardReviewPrompt)
	// context.Background(), NOT the caller's: the hook fires from a board
	// mutation made inside a live turn (agent board tool) or a WS read
	// loop (move op) — both contexts end independently of the reviewer,
	// and a cancelled parent context must not kill the review.
	_, sid, err := s.startTicketSession(context.Background(), &item, ticketStartOptions{
		label:         reviewSessionLabel(&item),
		prompt:        prompt,
		model:         model,
		thinkingLevel: thinking,
		claim:         false,
	})
	if err != nil {
		log.Printf("review agent: start for board item #%s failed: %v", item.ID, err)
		if _, cerr := bm.Comment(item.ID, fmt.Sprintf("review agent failed to start: %v", err), "review-agent"); cerr != nil {
			log.Printf("review agent: comment start failure on #%s: %v", item.ID, cerr)
		}
		return
	}
	log.Printf("review agent: session %s started for board item #%s (round %d)", sid, item.ID, item.ReviewRounds+1)
	s.broadcastBoardState()
	s.broadcastBoardNotice(fmt.Sprintf("Review agent started for board item #%s: %s", item.ID, item.Title))
}

// announceCap comments the cap notice on the ticket (once — see
// commentOnce).
func (t *boardReviewTrigger) announceCap(bm *agent.BoardManager, item *agent.BoardItem) {
	t.commentOnce(bm, item,
		fmt.Sprintf("auto-review stopped after %d review rounds; start a reviewer manually", agent.MaxReviewRounds))
}

// commentOnce comments a standing state on the ticket unless the activity
// log already carries the exact note — a ticket re-entering in_review
// must not spam identical notices on every move. The check reads the
// TICKET from the manager, not the hook's snapshot: the snapshot's
// activity log can be stale by the time the (post-unlock) hook runs.
func (t *boardReviewTrigger) commentOnce(bm *agent.BoardManager, item *agent.BoardItem, note string) {
	if fresh, err := bm.Item(item.ID); err == nil {
		for _, act := range fresh.Activity {
			if strings.TrimSpace(act.Text) == note {
				return
			}
		}
	}
	if _, err := bm.Comment(item.ID, note, "review-agent"); err != nil {
		log.Printf("review agent: comment on #%s: %v", item.ID, err)
	}
}

// reviewSessionLabel derives the reviewer's sidebar label (and board actor
// identity). Mirrors ticketSessionLabel with the review prefix.
func reviewSessionLabel(item *agent.BoardItem) string {
	label := fmt.Sprintf("review #%s: %s", item.ID, item.Title)
	if len(label) > 60 {
		label = label[:60] + "…"
	}
	return label
}

// isReviewActor reports whether by is the auto review session's label for
// the given ticket (the "review #<id>:" prefix — the label is also the
// board actor identity, so the reviewer's own board mutations carry it).
func isReviewActor(itemID, by string) bool {
	return strings.HasPrefix(by, fmt.Sprintf("review #%s:", itemID))
}
