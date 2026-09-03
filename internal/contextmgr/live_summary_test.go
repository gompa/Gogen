package contextmgr

import (
	"context"
	"os"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestLiveSummaryInstructionQuality is an opt-in real-model smoke test for
// the continuation-summary request shape (view prefix + head + middle +
// trailing USER-role instruction). It verifies summary QUALITY on a specific
// provider/model: the regression mode being watched is a model that treats
// the trailing instruction as a normal chat turn and answers the head
// question instead of recapping the middle (the failure mode that used to
// happen with a weakly framed trailing instruction).
//
// Run against the model you actually compact with, e.g.:
//
//	GOGEN_LIVE_SUMMARY=1 OPENAI_API_KEY=... OPENAI_MODEL=... OPENAI_BASE_URL=... \
//	    go test ./internal/contextmgr/ -run TestLiveSummaryInstructionQuality -v
//
// The -v output prints the produced summary for visual inspection; the
// assertions are heuristics (fact recall + no head-question answer) that
// catch the obvious regressions without over-constraining the model's
// wording.
func TestLiveSummaryInstructionQuality(t *testing.T) {
	if os.Getenv("GOGEN_LIVE_SUMMARY") == "" {
		t.Skip("set GOGEN_LIVE_SUMMARY=1 (plus OPENAI_API_KEY / OPENAI_MODEL / OPENAI_BASE_URL) to run the live summary smoke test")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		t.Fatal("OPENAI_MODEL is required for the live summary smoke test")
	}
	provider := llm.NewOpenAIProvider(os.Getenv("OPENAI_API_KEY"), model, os.Getenv("OPENAI_BASE_URL"), t.TempDir())
	m := NewManager(provider, Settings{
		ContextLimit:              128000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})

	// Head: a question the model must NOT answer in the summary (the
	// regression mode is "replied to the head question and echoed the
	// opening messages").
	// Middle: distinctive facts the summary MUST carry.
	head := llm.Message{Role: "user", Content: "What is the capital of Australia? Also, help me fix the auth middleware."}
	middle := []llm.Message{
		{Role: "assistant", Content: "I'll look at the auth middleware. The relevant file is internal/auth/middleware.go."},
		{Role: "tool", Content: strings.Repeat("func Middleware(next http.Handler) http.Handler { token := r.Header.Get(\"X-Auth-Token\")\n", 30) + "line 42: jwt.Parse failed: token expired"},
		{Role: "assistant", Content: "Found it: the middleware at internal/auth/middleware.go:42 rejected tokens without refreshing them. Decision: refresh on 401 via the token broker. The error is fixed by adding the refresh call."},
		{Role: "user", Content: "Good, now also bump the retry budget to 5."},
		{Role: "assistant", Content: "Done: retry budget is 5 in internal/auth/retry.go. Pending: add a regression test for the refresh path."},
	}
	tail := []llm.Message{
		{Role: "user", Content: "Now write the regression test."},
		{Role: "assistant", Content: "Starting on the test for the refresh path."},
	}
	msgs := append([]llm.Message{head}, middle...)
	msgs = append(msgs, tail...)

	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{
		ViewPrefix: []llm.Message{{Role: "system", Content: "You are a coding agent."}},
	})
	if err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	var summary string
	for _, msg := range out {
		if strings.HasPrefix(msg.Content, SummaryPrefix) {
			summary = msg.Content
		}
	}
	if summary == "" {
		t.Fatalf("no summary message in compacted history: %+v", out)
	}
	t.Logf("model %s produced this summary:\n%s", model, summary)

	low := strings.ToLower(summary)
	// Must recap the middle's facts.
	for _, want := range []string{"internal/auth/middleware.go", "refresh"} {
		if !strings.Contains(low, strings.ToLower(want)) {
			t.Errorf("summary missing middle fact %q:\n%s", want, summary)
		}
	}
	// Must NOT answer the head question (conversation-continuation
	// regression).
	if strings.Contains(low, "canberra") {
		t.Errorf("summary answered the head question instead of recapping the middle:\n%s", summary)
	}
}
