package agent

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// textOfTokens returns a message content whose estimated token count is at
// least n (and at most ~n+2). cl100k encodes repeated letters at ~8 chars
// per token, so the length is adjusted in whole-token chunks (a few
// tokenizations total) instead of a per-character or per-half-range search —
// the pure-Go tokenizer is slow on long strings (~30 KB/s).
func textOfTokens(t *testing.T, n int) string {
	t.Helper()
	count := func(s string) int {
		return contextmgr.ComputeMessageTokens(llm.Message{Role: "user", Content: s})
	}
	const charsPerToken = 8
	s := strings.Repeat("x", charsPerToken*n)
	c := count(s)
	for c < n {
		s += strings.Repeat("x", charsPerToken*(n-c)+charsPerToken)
		c = count(s)
	}
	for c > n {
		s = s[:max(0, len(s)-charsPerToken*(c-n))]
		c = count(s)
	}
	return s
}

// fitLoopEnv is the shared setup for the post-compaction fit-loop tests.
// The wire overhead (system prompt + tool definitions + project profile)
// rides on every request but is not in a.Messages, so the context limit is
// DERIVED from the measured overhead: with threshold 0.85 and reserve 4000,
// the limit is chosen so the "message budget" (CompactBudget minus the wire
// overhead) is exactly d, making the fit-loop math exact regardless of the
// prompt size. The overhead is measured AFTER seeding (measureOverhead), so
// lazily-resolved prompt parts (project profile, guidelines) are included —
// the same value prepareMessages sees. The manager is configured with
// keep-recent 2, so the shrinking-tail sequence under test is 2 -> 1 -> 0.
type fitLoopEnv struct {
	a        *Agent
	mgr      *contextmgr.Manager
	provider *countingProvider
	overhead int
	budget   int
	hard     int
	d        int // message budget: CompactBudget - wire overhead
	headTok  int // tokens of the "hello" head message
	sumTok   int // tokens of the framed "[Session summary...]" + "summary" message
}

// newFitLoopBase builds the agent (placeholder context limit). Call
// seedFitLoopConversation, then measureOverhead, then setBudget, then
// pinBaseline — in that order.
func newFitLoopBase(t *testing.T) *fitLoopEnv {
	t.Helper()
	provider := &countingProvider{limit: 1000000, fail: false}
	mgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              1000000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	return &fitLoopEnv{
		a:        NewAgent(provider, &Executor{WorkingDir: "."}, mgr),
		mgr:      mgr,
		provider: provider,
		headTok:  contextmgr.ComputeMessageTokens(llm.Message{Role: "user", Content: "hello"}),
		sumTok:   contextmgr.ComputeMessageTokens(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "summary"}),
	}
}

// seedFitLoopConversation appends the given messages and fills the
// per-message count cache. It does NOT pin the provider baseline —
// pinBaseline does, after the budget is fixed.
func (e *fitLoopEnv) seedFitLoopConversation(t *testing.T, msgs ...llm.Message) {
	t.Helper()
	for _, m := range msgs {
		e.a.appendMessage(m)
	}
	_ = e.a.ContextStats(context.Background())
}

// measureOverhead measures the wire overhead now that lazily-resolved prompt
// parts are built. It must run before setBudget and pinBaseline.
func (e *fitLoopEnv) measureOverhead(t *testing.T) {
	t.Helper()
	e.overhead = e.a.wireOverheadTokens()
	if e.overhead <= 0 {
		t.Fatalf("wire overhead = %d, want > 0", e.overhead)
	}
}

// setBudget fixes the context limit so the message budget (CompactBudget
// minus the wire overhead) is d: budget = 0.85*limit - 4000 = overhead + d.
func (e *fitLoopEnv) setBudget(t *testing.T, d int) {
	t.Helper()
	e.mgr.SetContextLimit(int(float64(e.overhead+d+4000) / 0.85))
	e.budget = e.mgr.CompactBudget()
	e.hard = e.mgr.HardLimit()
	e.d = e.budget - e.overhead
	if e.d < d-10 {
		t.Fatalf("message budget = %d, want ~%d (limit rounding)", e.d, d)
	}
}

// pinBaseline pins the estimated total to the exact wire cost (overhead +
// message counts) via a fresh provider baseline — so the trigger and
// fit-loop math is exact.
func (e *fitLoopEnv) pinBaseline(t *testing.T) {
	t.Helper()
	e.a.statsMu.RLock()
	total := e.overhead
	for _, c := range e.a.tokenCounts {
		total += c
	}
	e.a.statsMu.RUnlock()
	e.a.recordTurnUsage(&llm.Usage{PromptTokens: total, CompletionTokens: 10})
}

// checkSeed panics the test with the real numbers when the seeded total is
// not in [budget, hard) — the normal tier must fire and the emergency tier
// must not.
func (e *fitLoopEnv) checkSeed(t *testing.T) {
	t.Helper()
	total := e.a.compactionTokenTotal()
	if total < e.budget || total >= e.hard {
		t.Fatalf("test setup: total %d not in [%d, %d) (overhead %d, d %d)", total, e.budget, e.hard, e.overhead, e.d)
	}
}

// TestFitLoopSecondPassWithSmallerTail pins the core fix: when the first
// compaction pass succeeds but the post-compaction request is still over
// the budget (the preserved tail itself carries the bulk of the tokens),
// the fit loop repeats with a smaller tail — keep 2 -> 1 -> 0 — until the
// request fits.
func TestFitLoopSecondPassWithSmallerTail(t *testing.T) {
	e := newFitLoopBase(t)
	// Sizing (all in tokens; o = wire overhead, d = message budget):
	//   middle: 3 x m with 3m >= 500 (minMiddleTokens)
	//   tail:   T = d + 50 each, so
	//     pass 1 (keep 2) leaves o + head + summary + 2T  >= budget
	//     pass 2 (keep 1) leaves o + head + summary + T   >= budget
	//     pass 3 (keep 0) leaves o + head + summary       <  budget
	//   pre-compaction total o + head + 3m + 2T must stay BELOW the hard
	//   limit so only the normal tier fires:
	//     0.7d < 0.15*o + 515 - 0.85*head - 0.85*middleTotal
	//   and the retry middles (summary + T) must clear minMiddleTokens:
	//     d >= 450 - summary.
	const m = 167
	e.seedFitLoopConversation(t,
		llm.Message{Role: "user", Content: "hello"},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, 500)},
		llm.Message{Role: "user", Content: textOfTokens(t, 500)},
	)
	e.measureOverhead(t)
	dMin := 450 - e.sumTok
	dMax := int((0.15*float64(e.overhead) + 515 - 0.85*float64(e.headTok) - 0.85*float64(3*m)) / 0.7)
	if dMax <= dMin+10 {
		t.Fatalf("no feasible message budget for wire overhead %d (min %d, max %d)", e.overhead, dMin, dMax)
	}
	e.setBudget(t, (dMin+dMax)/2)
	// Re-seed the tails at the final size (d depends on the measured
	// overhead, which the placeholder tails already triggered).
	e.a.replaceMessages(nil)
	e.seedFitLoopConversation(t,
		llm.Message{Role: "user", Content: "hello"},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, e.d+50)},
		llm.Message{Role: "user", Content: textOfTokens(t, e.d+50)},
	)
	e.pinBaseline(t)
	e.checkSeed(t)

	e.a.prepareMessages(context.Background(), nil)

	calls := atomic.LoadInt64(&e.provider.calls)
	if calls != 3 {
		t.Fatalf("summarization calls = %d, want 3 (keep 2 -> 1 -> 0)", calls)
	}
	// keep=0 pass: only the head and the final summary remain.
	if len(e.a.Messages) != 2 {
		t.Fatalf("messages after fit loop = %d, want 2 (head + summary)", len(e.a.Messages))
	}
	if e.a.Messages[0].Content != "hello" {
		t.Fatalf("head message = %q, want the starting prompt preserved verbatim", e.a.Messages[0].Content)
	}
	if !contextmgr.IsCompactionSummary(e.a.Messages[1].Content) {
		t.Fatalf("second message = %q, want a compaction summary", e.a.Messages[1].Content)
	}
	if total := e.a.compactionTokenTotal(); total >= e.budget {
		t.Fatalf("total after fit loop = %d, want < budget %d", total, e.budget)
	}
	if backoff, _ := compactState(e.a); !backoff.IsZero() {
		t.Fatalf("backoff set after the fit loop reached the budget: %v", backoff)
	}
}

// TestFitLoopStopsWhenBudgetReached pins the regression guard: when the
// first pass already lands the request under the budget, no extra
// summarization calls happen — the single-pass behavior is unchanged.
func TestFitLoopStopsWhenBudgetReached(t *testing.T) {
	e := newFitLoopBase(t)
	// d is just above head + summary + the two 300-token tail messages, so
	// the first pass (keep 2) fits; the pre-compaction total is over the
	// budget because of the three 300-token middle messages.
	e.seedFitLoopConversation(t,
		llm.Message{Role: "user", Content: "hello"},
		llm.Message{Role: "user", Content: textOfTokens(t, 300)},
		llm.Message{Role: "user", Content: textOfTokens(t, 300)},
		llm.Message{Role: "user", Content: textOfTokens(t, 300)},
		llm.Message{Role: "user", Content: textOfTokens(t, 300)},
		llm.Message{Role: "user", Content: textOfTokens(t, 300)},
	)
	e.measureOverhead(t)
	e.setBudget(t, e.headTok+e.sumTok+700)
	e.pinBaseline(t)
	e.checkSeed(t)

	e.a.prepareMessages(context.Background(), nil)

	calls := atomic.LoadInt64(&e.provider.calls)
	if calls != 1 {
		t.Fatalf("summarization calls = %d, want 1 (first pass already fits)", calls)
	}
	// head + summary + tail(2): both recent messages survive verbatim.
	if len(e.a.Messages) != 4 {
		t.Fatalf("messages after compaction = %d, want 4 (head + summary + tail 2)", len(e.a.Messages))
	}
	if !contextmgr.IsCompactionSummary(e.a.Messages[1].Content) {
		t.Fatalf("second message = %q, want a compaction summary", e.a.Messages[1].Content)
	}
	if total := e.a.compactionTokenTotal(); total >= e.budget {
		t.Fatalf("total after compaction = %d, want < budget %d", total, e.budget)
	}
	if backoff, _ := compactState(e.a); !backoff.IsZero() {
		t.Fatalf("backoff set after a successful single pass: %v", backoff)
	}
}

// TestFitLoopExhaustedGivesUpIntoBackoff pins the give-up path: when every
// pass in the shrinking-tail sequence succeeds but the request is still
// over budget (the preserved pre-head system message alone keeps the total
// over the budget), the loop stops after at most 3 summarization calls,
// gives up cleanly into the failure backoff, and does not hang.
func TestFitLoopExhaustedGivesUpIntoBackoff(t *testing.T) {
	e := newFitLoopBase(t)
	// The pre-head system message is preserved verbatim by every pass.
	// Sizing (o = wire overhead, d = message budget, S = system tokens):
	//   S = d - 10, so the keep=0 result o + S + head + summary >= budget
	//   pre total o + S + head + 3m + 2T < hard limit requires
	//     0.15*(o + d) > 650, i.e. d > 650/0.15 - o
	//   retry middles (summary + T) must clear minMiddleTokens: T >= 450 - summary
	const m = 167
	const tail = 490
	d := int(math.Ceil(650/0.15)) - e.overhead + 500
	e.seedFitLoopConversation(t,
		llm.Message{Role: "system", Content: textOfTokens(t, d-10)},
		llm.Message{Role: "user", Content: "hello"},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, m)},
		llm.Message{Role: "user", Content: textOfTokens(t, tail)},
		llm.Message{Role: "user", Content: textOfTokens(t, tail)},
	)
	e.measureOverhead(t)
	e.setBudget(t, d)
	e.pinBaseline(t)
	e.checkSeed(t)

	e.a.prepareMessages(context.Background(), nil)

	calls := atomic.LoadInt64(&e.provider.calls)
	if calls != 3 {
		t.Fatalf("summarization calls = %d, want 3 (capped at keep 2 -> 1 -> 0)", calls)
	}
	// The loop ran to the end of the sequence: system + head + final summary.
	if len(e.a.Messages) != 3 {
		t.Fatalf("messages after exhausted fit loop = %d, want 3 (system + head + summary)", len(e.a.Messages))
	}
	if e.a.Messages[1].Content != "hello" {
		t.Fatalf("head message = %q, want the starting prompt preserved verbatim", e.a.Messages[1].Content)
	}
	if !contextmgr.IsCompactionSummary(e.a.Messages[2].Content) {
		t.Fatalf("third message = %q, want a compaction summary", e.a.Messages[2].Content)
	}
	// Gave up into the failure backoff so the next turn does not
	// immediately repeat the whole sequence.
	backoff, _ := compactState(e.a)
	if backoff.IsZero() {
		t.Fatal("expected the failure backoff to be active after the exhausted fit loop")
	}
}
