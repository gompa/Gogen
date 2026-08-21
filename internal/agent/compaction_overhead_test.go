package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// newOverheadTestAgent builds an agent with a large context window so the
// compaction budget sits far above the wire overhead, letting tests place
// the message total in the gap between the two.
func newOverheadTestAgent(t *testing.T) (*Agent, *contextmgr.Manager) {
	t.Helper()
	provider := &statsStubProvider{limit: 100000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              100000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	return a, ctxMgr
}

// fillMessagesTo appends repeated messages until the local per-message token
// total reaches at least target, returning the actual total.
func fillMessagesTo(t *testing.T, a *Agent, target int) int {
	t.Helper()
	total := 0
	for total < target {
		m := llm.Message{Role: "user", Content: strings.Repeat("x", 400)}
		total += contextmgr.ComputeMessageTokens(m)
		a.appendMessage(m)
	}
	return total
}

// TestShouldCompactOverheadAccounting pins the wire-overhead accounting in
// shouldCompactUsingCounts. The message total is placed in the gap between
// the compaction budget and budget+overhead, so the decision flips purely on
// whether the overhead (system prompt + tool definitions) is counted:
//   - baseline absent (post-compaction / post-restore / first turn): added
//   - baseline fresh (provider prompt_tokens): NOT added (double-count trap)
//   - the same rules apply on the !complete fallback path
func TestShouldCompactOverheadAccounting(t *testing.T) {
	a, ctxMgr := newOverheadTestAgent(t)

	budget := ctxMgr.CompactBudget()
	if budget <= 0 {
		t.Fatalf("budget = %d, want > 0 (auto-compaction must be enabled)", budget)
	}
	overhead := a.wireOverheadTokens()
	if overhead <= 0 {
		t.Fatalf("wire overhead = %d, want > 0", overhead)
	}
	// Place the local total below the budget but above it once the overhead
	// is added, so each scenario's expectation is discriminating.
	localTotal := fillMessagesTo(t, a, budget-overhead/2)
	if localTotal >= budget {
		t.Fatalf("setup: local total %d already at budget %d", localTotal, budget)
	}
	if localTotal+overhead < budget {
		t.Fatalf("setup: local total %d + overhead %d below budget %d", localTotal, overhead, budget)
	}

	// Fill the per-message counts cache (the complete path).
	_ = a.ContextStats(context.Background())

	tests := []struct {
		name  string
		setup func()
		want  bool
	}{
		{
			name:  "baseline absent, cached counts: overhead added",
			setup: func() { a.clearTurnUsage() },
			want:  true,
		},
		{
			name: "baseline fresh, cached counts: overhead NOT added again",
			setup: func() {
				// The provider count already covers system prompt + tools;
				// just under the budget. Adding the overhead on top would
				// wrongly flip the decision to true.
				a.recordTurnUsage(&llm.Usage{PromptTokens: budget - 1, CompletionTokens: 10})
			},
			want: false,
		},
		{
			name: "baseline absent, !complete fallback: overhead added",
			setup: func() {
				a.clearTurnUsage()
				a.replaceMessages(a.Messages) // clears the counts cache
			},
			want: true,
		},
		{
			name: "baseline fresh, !complete fallback: overhead NOT added again",
			setup: func() {
				a.replaceMessages(a.Messages) // clears the counts cache
				a.recordTurnUsage(&llm.Usage{PromptTokens: budget - 1, CompletionTokens: 10})
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			if got := a.shouldCompactUsingCounts(); got != tc.want {
				t.Fatalf("shouldCompactUsingCounts() = %v, want %v (local=%d overhead=%d budget=%d)",
					got, tc.want, localTotal, overhead, budget)
			}
		})
	}
}

// TestWireOverheadCacheInvalidation pins the fingerprint-keyed overhead
// cache: unchanged inputs are a cache hit, and any change to the system
// prompt or the tool definitions invalidates it.
func TestWireOverheadCacheInvalidation(t *testing.T) {
	a, _ := newOverheadTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hi"})

	base := a.wireOverheadTokens()
	if base <= 0 {
		t.Fatalf("wire overhead = %d, want > 0", base)
	}

	// Unchanged inputs: cache hit, same value.
	if got := a.wireOverheadTokens(); got != base {
		t.Fatalf("overhead changed without input change: %d -> %d", base, got)
	}

	// System prompt change (project guidelines) invalidates the cache.
	a.ProjectGuidelines = strings.Repeat("rule ", 200)
	withGuidelines := a.wireOverheadTokens()
	if withGuidelines <= base {
		t.Fatalf("guidelines change did not grow overhead: %d -> %d", base, withGuidelines)
	}

	// Tool definition change (MCP tool added) invalidates the cache.
	a.MCPRegistry = &fakeMCPRegistry{
		names: map[string]struct{}{"mcp_extra": {}},
		defs: []llm.Tool{{
			Type:        "function",
			Name:        "mcp_extra",
			Description: strings.Repeat("mcp ", 200),
		}},
	}
	withTool := a.wireOverheadTokens()
	if withTool <= withGuidelines {
		t.Fatalf("tool change did not grow overhead: %d -> %d", withGuidelines, withTool)
	}

	// Restoring the tool set recomputes back to the guidelines-only value.
	a.MCPRegistry = nil
	if got := a.wireOverheadTokens(); got != withGuidelines {
		t.Fatalf("restoring tools: overhead = %d, want %d", got, withGuidelines)
	}
}

// TestShouldCompactOverheadRegressionGuard pins the regression property: in
// baseline-absent states the new accounting can only fire the trigger
// earlier, never later — the overhead is non-negative and is only ever
// added on top of the pre-change total.
func TestShouldCompactOverheadRegressionGuard(t *testing.T) {
	a, ctxMgr := newOverheadTestAgent(t)

	overhead := a.wireOverheadTokens()
	if overhead < 0 {
		t.Fatalf("wire overhead = %d, want >= 0", overhead)
	}
	// A total that would NOT trigger without the overhead: with it, the
	// decision is the same or earlier (here: flips to true).
	localTotal := fillMessagesTo(t, a, ctxMgr.CompactBudget()-overhead/2)
	a.clearTurnUsage()
	if !a.shouldCompactUsingCounts() {
		t.Fatalf("expected the trigger to fire with overhead (local=%d overhead=%d budget=%d)",
			localTotal, overhead, ctxMgr.CompactBudget())
	}
	// And a total far below budget-overhead never triggers, with or without
	// the overhead (the trigger cannot fire later than before the change).
	a2, _ := newOverheadTestAgent(t)
	fillMessagesTo(t, a2, ctxMgr.CompactBudget()-a2.wireOverheadTokens()-1000)
	a2.clearTurnUsage()
	if a2.shouldCompactUsingCounts() {
		t.Fatal("trigger fired below budget-overhead; accounting is not monotone")
	}
}
