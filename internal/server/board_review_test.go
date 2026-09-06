package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// TestReviewAgentViaWS drives the full auto-review flow over the board
// channel: with the review agent on, a move into in_review spawns a
// headless review session seeded with the review prompt — without
// claiming the ticket (the worker holds the assignment) — and the ticket
// carries the review link + rounds counter.
func TestReviewAgentViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{
			{ID: "default-model", ContextLimit: 128000, Current: true},
			{ID: "reviewer-model", ContextLimit: 64000},
		}
		p.StreamResults = []*llm.StreamResult{{Content: "reviewed, looks good"}}
		return p
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	// Enable the board AND the review agent in one toggle.
	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "on", ReviewAgent: "on", SessionID: sid}); err != nil {
		t.Fatalf("send board+review on: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.Board == "on" && m.ReviewAgent == "on"
	})

	// Add a card, then move it into in_review (the auto-review trigger).
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "add", Title: "Fix parser crash", Description: "make go test pass", Priority: "high"}}); err != nil {
		t.Fatalf("send board_op add: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "board_state" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })

	// The trigger's own broadcasts interleave with the move op's (the hook
	// fires while the move op is handled), so collect everything until the
	// move ack and assert over the set.
	if err := conn.WriteJSON(WSMessage{Type: "board_op", BoardOp: &BoardOpRequest{Action: "move", ID: "1", Column: "in_review"}}); err != nil {
		t.Fatalf("send board_op move: %v", err)
	}
	frames := readMessagesUntil(t, conn, 10*time.Second, func(msgs []WSMessage) bool {
		for _, m := range msgs {
			if m.Type == "notice" && strings.Contains(m.Content, "Moved board item #1 to in_review") {
				return true
			}
		}
		return false
	})
	var reviewNotice bool
	var reviewItem *agent.BoardItem
	for _, m := range frames {
		switch {
		case m.Type == "notice" && strings.Contains(m.Content, "Review agent started for board item #1"):
			reviewNotice = true
		case m.Type == "board_state" && m.BoardState != nil && len(m.BoardState.Items) > 0 &&
			m.BoardState.Items[0].ReviewSessionID != "":
			item := m.BoardState.Items[0]
			reviewItem = &item
		}
	}
	if !reviewNotice {
		t.Fatalf("no review-agent notice in %d frames", len(frames))
	}
	if reviewItem == nil {
		t.Fatal("no board_state carried the review session link")
	}
	if reviewItem.ReviewRounds != 1 {
		t.Fatalf("review rounds = %d, want 1", reviewItem.ReviewRounds)
	}
	if reviewItem.Status != "in_review" || reviewItem.Assignee != "" {
		t.Fatalf("reviewer claimed the ticket: status %q assignee %q, want in_review/unassigned", reviewItem.Status, reviewItem.Assignee)
	}

	// The reviewer session runs the review prompt as its first user
	// message, on the cascade's resolved model (workspace default here).
	reviewSid := reviewItem.ReviewSessionID
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), reviewSid)
		if err != nil {
			return false
		}
		if snap.Model != "default-model" {
			t.Errorf("reviewer model = %q, want default-model", snap.Model)
			return true
		}
		return len(snap.Messages) > 0 && snap.Messages[0].Role == "user" &&
			strings.Contains(snap.Messages[0].Content, "You are the review agent for board ticket #1: Fix parser crash")
	})
}

// TestReviewAgentModelCascade pins the review model cascade at the
// trigger: the ticket's own review model override wins over the
// configured review_agent_model, which wins over the workspace default
// (no explicit selection — the fresh provider's seed).
func TestReviewAgentModelCascade(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{
			{ID: "default-model", ContextLimit: 128000, Current: true},
			{ID: "cfg-model", ContextLimit: 64000},
			{ID: "ticket-model", ContextLimit: 32000},
		}
		p.StreamResults = []*llm.StreamResult{{Content: "reviewed"}}
		return p
	}
	bm := s.ws.ensureBoardManager()
	s.installReviewAgent()
	s.ws.SetReviewAgentEnabled(true)
	r := s.ws.GetRuntimeConfig()
	r.ReviewAgentModel = "cfg-model"
	s.ws.SetRuntimeConfig(r)

	if _, err := bm.Add("Fix parser crash", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	// finishedReviewer waits for the linked reviewer's turn to have
	// completed (assistant reply persisted) and asserts its model. The
	// register-time flush persists the provider SEED model; the selected
	// model is persisted by the turn's final flush, so the assertion must
	// wait past it.
	finishedReviewer := func(wantModel string) {
		t.Helper()
		waitFor(t, 10*time.Second, func() bool {
			item, err := bm.Item("1")
			if err != nil || item.ReviewSessionID == "" {
				return false
			}
			snap, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), item.ReviewSessionID)
			if err != nil {
				return false
			}
			hasReply := false
			for _, msg := range snap.Messages {
				if msg.Role == "assistant" {
					hasReply = true
				}
			}
			if !hasReply {
				return false
			}
			if snap.Model != wantModel {
				t.Errorf("reviewer model = %q, want %s", snap.Model, wantModel)
			}
			return true
		})
	}
	// Configured tier: no per-ticket override → cfg-model.
	trig := &boardReviewTrigger{s: s}
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review"}, "user")
	finishedReviewer("cfg-model")

	// Per-ticket override wins: ticket-model (the crafted snapshot has no
	// link, so the duplicate guard does not fire even though the first
	// reviewer is still linked).
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review", ReviewModel: "ticket-model"}, "user")
	finishedReviewer("ticket-model")
}

// TestReviewAgentGuards pins the trigger guards at the hook: feature flag
// off, the reviewer's own activity (recursion), a live reviewer (one per
// ticket), and the review-rounds cap (announced once on the ticket).
func TestReviewAgentGuards(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	// The default test ProviderFactory returns the shared blocking stub:
	// a spawned reviewer's turn never finishes, so the runtime stays in
	// the registry and session-count assertions are deterministic.
	bm := s.ws.ensureBoardManager()
	s.installReviewAgent()
	trig := &boardReviewTrigger{s: s}
	if _, err := bm.Add("Fix parser crash", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	liveSessions := func() int { return len(s.registry.activeIDs()) }

	// Flag off: nothing spawns.
	s.ws.SetReviewAgentEnabled(false)
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review"}, "user")
	if got := liveSessions(); got != 1 {
		t.Fatalf("flag off spawned a reviewer (%d live sessions)", got)
	}

	// Flag on: the spawn works (default test provider, no model cascade —
	// the workspace default model resolves it).
	s.ws.SetReviewAgentEnabled(true)
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review"}, "user")
	if got := liveSessions(); got != 2 {
		t.Fatalf("expected the reviewer spawned (live sessions = %d, want 2)", got)
	}
	item, err := bm.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	reviewSid := item.ReviewSessionID
	if reviewSid == "" {
		t.Fatal("no review link after spawn")
	}

	// Recursion: a move BY the reviewer itself never spawns another.
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review", ReviewSessionID: reviewSid}, reviewSessionLabel(&agent.BoardItem{ID: "1", Title: "Fix parser crash"}))
	if got := liveSessions(); got != 2 {
		t.Fatalf("recursion guard failed (live sessions = %d, want 2)", got)
	}

	// Duplicate: the linked review session still live → no second
	// reviewer for the ticket.
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review", ReviewSessionID: reviewSid}, "user")
	if got := liveSessions(); got != 2 {
		t.Fatalf("duplicate guard failed (live sessions = %d, want 2)", got)
	}

	// Cap: at MaxReviewRounds the trigger comments the ticket instead of
	// spawning — and announces the cap only once on later moves.
	capped := agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review", ReviewRounds: agent.MaxReviewRounds}
	trig.maybeStartReview(capped, "user")
	if got := liveSessions(); got != 2 {
		t.Fatalf("cap guard failed (live sessions = %d, want 2)", got)
	}
	trig.maybeStartReview(capped, "user")
	item, err = bm.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	capComments := 0
	for _, act := range item.Activity {
		if strings.Contains(act.Text, "auto-review stopped after") {
			capComments++
		}
	}
	if capComments != 1 {
		t.Fatalf("cap announced %d times, want 1 (activity %+v)", capComments, item.Activity)
	}
}

// TestReviewAgentNoModel pins the no-model guard: with no workspace
// default and no cascade, the trigger refuses before creating anything
// and comments the ticket instead of leaving a headless session that
// would fail silently.
func TestReviewAgentNoModel(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	bm := s.ws.ensureBoardManager()
	s.installReviewAgent()
	s.ws.SetReviewAgentEnabled(true)
	// The test workspace seeds its default model from the initial agent
	// (the blocking stub): clear it so the cascade is genuinely empty,
	// like a workspace with no model configured.
	s.ws.SetDefaultModel("")
	if _, err := bm.Add("Fix parser crash", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	before := liveSessionFiles(t, s)
	trig := &boardReviewTrigger{s: s}
	trig.maybeStartReview(agent.BoardItem{ID: "1", Title: "Fix parser crash", Status: "in_review"}, "user")
	if after := liveSessionFiles(t, s); after != before {
		t.Fatalf("no-model spawn created sessions (%d → %d)", before, after)
	}
	item, err := bm.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, act := range item.Activity {
		if strings.Contains(act.Text, "review agent did not start") && strings.Contains(act.Text, "no model configured") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no-model failure not commented on the ticket (activity %+v)", item.Activity)
	}
}

// liveSessionFiles counts the persisted session files (a lower-bound
// "did a session get created" signal that does not depend on registry
// eviction timing).
func liveSessionFiles(t *testing.T, s *Server) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.ws.GetWorkingDir(), ".gogen", "sessions"))
	if err != nil {
		return 0
	}
	return len(entries)
}
