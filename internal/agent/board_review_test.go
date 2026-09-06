package agent

import (
	"strings"
	"testing"
	"time"
)

// TestMoveReviewHook pins the auto-review trigger semantics on the manager:
// the hook fires exactly on a successful Move into in_review — the single
// choke point both the agent board tool and the web UI go through — with a
// snapshot of the moved ticket and the actor label. Claim/Done/Block and
// moves to other columns never fire it.
func TestMoveReviewHook(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("Fix parser crash", "make go test pass", "high", "user"); err != nil {
		t.Fatal(err)
	}

	var fired []BoardItem
	var actors []string
	m.SetOnReviewNeeded(func(item BoardItem, by string) {
		fired = append(fired, item)
		actors = append(actors, by)
	})

	// Claim (→ in_progress), block, and an in_progress move: no hook.
	if _, err := m.Claim("1", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move("1", "backlog", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Block("1", "waiting", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Done("1", "user"); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 0 {
		t.Fatalf("hook fired %d times before any in_review move", len(fired))
	}

	// The in_review move fires it once, with the moved item + actor.
	if _, err := m.Move("1", "in_review", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(fired))
	}
	if fired[0].ID != "1" || fired[0].Status != "in_review" || actors[0] != "agent-a" {
		t.Fatalf("hook args = item %+v actor %q, want item #1 in_review by agent-a", fired[0], actors[0])
	}

	// A move within in_review (already there) is a no-op return: no hook.
	if _, err := m.Move("1", "in_review", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 {
		t.Fatalf("no-op move fired the hook (%d times total)", len(fired))
	}
}

// TestMoveReviewHookFiresOutsideLock pins the lock contract: the hook runs
// after the manager lock is released, so its handler may call back into the
// manager (Comment here) without deadlocking. A regression (hook under
// m.mu) hangs this test until the go-test timeout.
func TestMoveReviewHookFiresOutsideLock(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("Fix parser crash", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.SetOnReviewNeeded(func(item BoardItem, by string) {
		defer close(done)
		// Re-entrant manager call from inside the hook.
		if _, err := m.Comment(item.ID, "reviewing", by); err != nil {
			t.Errorf("comment from inside the hook: %v", err)
		}
	})
	if _, err := m.Move("1", "in_review", "agent-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("review hook did not run (deadlock?)")
	}
}

// TestMoveLeavingReviewClearsLink pins the review-link lifecycle: the link
// is set on a move into in_review (by AttachReviewAgent) and cleared by any
// move out of it, so the next entry can start a fresh reviewer.
func TestMoveLeavingReviewClearsLink(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("Fix parser crash", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move("1", "in_review", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AttachReviewAgent("1", "sess-1", "review #1: x"); err != nil {
		t.Fatal(err)
	}
	item, err := m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.ReviewSessionID != "sess-1" || item.ReviewRounds != 1 {
		t.Fatalf("after attach: link %q rounds %d, want sess-1/1", item.ReviewSessionID, item.ReviewRounds)
	}
	// Leaving in_review clears the link (but not the rounds counter).
	if _, err := m.Move("1", "in_progress", "agent-a"); err != nil {
		t.Fatal(err)
	}
	item, err = m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.ReviewSessionID != "" || item.ReviewRounds != 1 {
		t.Fatalf("after leaving review: link %q rounds %d, want \"\"/1", item.ReviewSessionID, item.ReviewRounds)
	}
}

// TestSetReviewOptions pins the per-ticket review options: the level is
// canonicalized (NormalizeThinkingLevel), and an empty op clears both
// fields back to the inherited defaults.
func TestSetReviewOptions(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("Fix parser crash", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetReviewOptions("1", " model-2 ", "HIGH", "user"); err != nil {
		t.Fatal(err)
	}
	item, err := m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.ReviewModel != "model-2" || item.ReviewThinkingLevel != "high" {
		t.Fatalf("review options = %q/%q, want model-2/high", item.ReviewModel, item.ReviewThinkingLevel)
	}
	// Empty clears (the popover's "inherit" state).
	if _, err := m.SetReviewOptions("1", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	item, err = m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.ReviewModel != "" || item.ReviewThinkingLevel != "" {
		t.Fatalf("cleared review options = %q/%q, want empty", item.ReviewModel, item.ReviewThinkingLevel)
	}
}

// TestReviewPrompt pins the review seed prompt: the built-in default's
// placeholder substitution (single-pass, like TicketPrompt), the built-in
// default resolution for an empty configured template, and custom template
// passthrough with the assignee fallback.
func TestReviewPrompt(t *testing.T) {
	item := &BoardItem{
		ID:          "7",
		Title:       "Fix {description} crash",
		Description: "the parser panics on empty input",
		Priority:    "",
		Assignee:    "",
		Activity: []BoardActivity{
			{At: time.Now(), By: "agent-a", Text: "claimed"},
			{At: time.Now(), By: "agent-a", Text: "implemented the fix"},
		},
	}
	got := ReviewPrompt(item, "")
	if !strings.Contains(got, "You are the review agent for board ticket #7: Fix {description} crash") {
		t.Fatalf("title not substituted verbatim: %q", got)
	}
	// Single-pass: the title's own "{description}" must NOT be re-run.
	if strings.Count(got, "the parser panics on empty input") != 1 {
		t.Fatalf("description substitution is not single-pass: %q", got)
	}
	if !strings.Contains(got, "Priority: none") {
		t.Fatalf("empty priority falls back to none: %q", got)
	}
	if !strings.Contains(got, "moved to in_review by the assignee") {
		t.Fatalf("empty assignee falls back: %q", got)
	}
	if !strings.Contains(got, "- implemented the fix") {
		t.Fatalf("activity context missing: %q", got)
	}
	if strings.Contains(got, "- claimed") {
		t.Fatalf("transition noise leaked into the context: %q", got)
	}

	// Custom template: resolved verbatim, only listed placeholders run.
	custom := ReviewPrompt(item, "Review #{id}: {assignee} says {title}")
	if custom != "Review #7: the assignee says Fix {description} crash" {
		t.Fatalf("custom template render = %q", custom)
	}

	// Named assignee.
	item.Assignee = "ticket #7: agent-a"
	got = ReviewPrompt(item, "")
	if !strings.Contains(got, "moved to in_review by ticket #7: agent-a") {
		t.Fatalf("named assignee not substituted: %q", got)
	}
}
