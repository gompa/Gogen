package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// newPreflightTestAgent builds an agent with a placeholder context limit;
// each test derives the real limit from the measured wire overhead so the
// pre-flight math (estimate vs ContextLimit) is exact regardless of the
// system prompt / tool set size.
func newPreflightTestAgent(t *testing.T) (*Agent, *contextmgr.Manager, *countingProvider) {
	t.Helper()
	provider := &countingProvider{limit: 1000000, fail: false}
	mgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              1000000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, mgr)
	return a, mgr, provider
}

// setLimitOverEstimate derives the context limit so the outgoing estimate
// (wire overhead + cached message counts, the exact full-view estimate)
// lands over tokens ABOVE the window: the request would be refused.
func setLimitOverEstimate(t *testing.T, a *Agent, mgr *contextmgr.Manager, over int) {
	t.Helper()
	a.statsMu.RLock()
	msgTok := 0
	for _, c := range a.tokenCounts {
		msgTok += c
	}
	a.statsMu.RUnlock()
	mgr.SetContextLimit(a.wireOverheadTokens() + msgTok - over)
}

// assertToolProtocol verifies the tool-call/result protocol of an outgoing
// view: every tool result has a preceding assistant message that called it,
// and every assistant tool call has all of its results present (an outgoing
// request must never end with a pending call).
func assertToolProtocol(t *testing.T, msgs []llm.Message) {
	t.Helper()
	needed := map[string]int{}
	seen := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				seen[tc.ID] = true
				needed[tc.ID]++
			}
		case "tool":
			if !seen[m.ToolCallID] {
				t.Fatalf("message %d: tool result %q has no calling assistant message in the view", i, m.ToolCallID)
			}
			needed[m.ToolCallID]--
			if needed[m.ToolCallID] < 0 {
				t.Fatalf("message %d: duplicate tool result for call %q", i, m.ToolCallID)
			}
		}
	}
	for id, n := range needed {
		if n > 0 {
			t.Fatalf("tool call %q has %d pending result(s): the request would end mid-protocol", id, n)
		}
	}
}

// TestPreflightMidLoopForcedCompaction pins the core fix: growth inside the
// tool loop (the last message is a tool result, so the boundary check never
// runs) pushes the outgoing request past the context limit — the pre-flight
// check fires the forced compaction (keep=0: head + summary, no tail) and
// the outgoing view keeps the tool protocol intact.
func TestPreflightMidLoopForcedCompaction(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	for i := 0; i < 4; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 400)})
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	_ = a.ContextStats(context.Background())
	setLimitOverEstimate(t, a, mgr, 50)

	view, _ := a.prepareMessages(context.Background(), nil)

	if got := atomic.LoadInt64(&provider.calls); got == 0 {
		t.Fatal("no summarization calls: the pre-flight forced compaction did not run")
	}
	// keep=0: head + summary, plus the trailing tool group (the in-flight
	// tool round) preserved verbatim.
	if len(a.Messages) != 4 {
		t.Fatalf("messages after forced compaction = %d, want 4 (head + summary + tail 2)", len(a.Messages))
	}
	if a.Messages[0].Content != "hello" {
		t.Fatalf("head = %q, want the starting prompt preserved verbatim", a.Messages[0].Content)
	}
	// The summary is a plain assistant message with no tool calls.
	if sum := a.Messages[1]; sum.Role != "assistant" || len(sum.ToolCalls) != 0 || !contextmgr.IsCompactionSummary(sum.Content) {
		t.Fatalf("second message = role %q, %d tool calls, summary=%v; want a plain assistant summary",
			sum.Role, len(sum.ToolCalls), contextmgr.IsCompactionSummary(sum.Content))
	}
	// The tail starts at the calling assistant message (walk-back over the
	// trailing tool result) and carries the fresh result verbatim.
	if m := a.Messages[2]; m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tail start = role %q (tool calls %d), want the calling assistant message (non-tool)", m.Role, len(m.ToolCalls))
	}
	if m := a.Messages[3]; m.Role != "tool" || m.ToolCallID != "call_1" {
		t.Fatalf("tail end = role %q id %q, want the trailing tool result", m.Role, m.ToolCallID)
	}
	assertToolProtocol(t, view)
	if est := a.outgoingViewEstimate(view); est >= mgr.ContextLimit() {
		t.Fatalf("estimate after forced compaction = %d, want < limit %d", est, mgr.ContextLimit())
	}
}

// TestPreflightMidLoopPinnedTailProtocol pins the mid-loop protocol safety
// with a non-empty tail: a pinned tool result pulls the tail back, and the
// tail must start at the CALLING assistant message (adjustCompactTailStart
// walks back over the trailing tool result) so the tool-call/result pair
// survives verbatim.
func TestPreflightMidLoopPinnedTailProtocol(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 800)})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 800)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_2", Name: "read_file", Args: map[string]interface{}{"path": "main.go"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_2", Content: "package main"})
	_ = a.ContextStats(context.Background())
	setLimitOverEstimate(t, a, mgr, 50)
	pm := NewPinManager()
	a.PinManager = pm
	pm.ReplacePins(map[int]struct{}{6: {}}) // pin the last tool result

	view, _ := a.prepareMessages(context.Background(), nil)

	if got := atomic.LoadInt64(&provider.calls); got == 0 {
		t.Fatal("no summarization calls: the pre-flight forced compaction did not run")
	}
	// head + summary + the pinned tail, which must start at the calling
	// assistant message (walk-back over the trailing tool result).
	if len(a.Messages) != 4 {
		t.Fatalf("messages after forced compaction = %d, want 4 (head + summary + tail 2)", len(a.Messages))
	}
	if m := a.Messages[2]; m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_2" {
		t.Fatalf("tail start = role %q (tool calls %d), want the calling assistant message (non-tool)", m.Role, len(m.ToolCalls))
	}
	if m := a.Messages[3]; m.Role != "tool" || m.ToolCallID != "call_2" {
		t.Fatalf("tail end = role %q id %q, want the pinned tool result", m.Role, m.ToolCallID)
	}
	assertToolProtocol(t, view)
	if est := a.outgoingViewEstimate(view); est >= mgr.ContextLimit() {
		t.Fatalf("estimate after forced compaction = %d, want < limit %d", est, mgr.ContextLimit())
	}
}

// TestPreflightCappingOnlyNoLLMCall pins the model-free stage: when capping
// oversized tool results alone brings the request under the window, the
// pre-flight check stops there — no summarization call is made. The view is
// built WITHOUT capping (prepareMessages caps before the estimate), so the
// pre-flight method is exercised directly with the over-limit view.
func TestPreflightCappingOnlyNoLLMCall(t *testing.T) {
	provider := &countingProvider{limit: 1000000, fail: false}
	mgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              1000000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
		MaxToolResultBytes:        1000,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, mgr)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "execute_command", Args: map[string]interface{}{"command": "ls -la"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: strings.Repeat("y", 30000)})
	_ = a.ContextStats(context.Background())
	setLimitOverEstimate(t, a, mgr, 50)

	view := a.rebuildView()
	if est := a.outgoingViewEstimate(view); est < mgr.ContextLimit() {
		t.Fatalf("setup: estimate %d not over limit %d", est, mgr.ContextLimit())
	}
	out, _ := a.preflightForcedCompact(context.Background(), nil, view)

	if got := atomic.LoadInt64(&provider.calls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 (capping-only win)", got)
	}
	if len(a.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (capping must not summarize)", len(a.Messages))
	}
	if len(a.Messages[2].Content) > 1000 {
		t.Fatalf("tool body = %d bytes, want capped at 1000", len(a.Messages[2].Content))
	}
	if est := a.outgoingViewEstimate(out); est >= mgr.ContextLimit() {
		t.Fatalf("estimate after capping = %d, want < limit %d", est, mgr.ContextLimit())
	}
}

// TestPreflightStrictShrinkAbort pins the strict-shrink requirement: a
// forced compaction whose result is not strictly smaller than the input
// (a single-message middle compacts to the same length) is aborted — the
// history is left untouched and the view is handed off as-is.
func TestPreflightStrictShrinkAbort(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 800)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	_ = a.ContextStats(context.Background())
	setLimitOverEstimate(t, a, mgr, 50)
	pm := NewPinManager()
	a.PinManager = pm
	pm.ReplacePins(map[int]struct{}{3: {}}) // pin the tool result: middle is the single big message
	before := len(a.Messages)

	a.prepareMessages(context.Background(), nil)

	if got := atomic.LoadInt64(&provider.calls); got == 0 {
		t.Fatal("no summarization calls: the forced compaction did not run")
	}
	// 4 -> 4: no shrink, so the compaction is aborted and the history is
	// untouched.
	if len(a.Messages) != before {
		t.Fatalf("messages = %d, want %d (strict-shrink abort)", len(a.Messages), before)
	}
	if contextmgr.IsCompactionSummary(a.Messages[1].Content) {
		t.Fatal("a summary was inserted despite the strict-shrink abort")
	}
}

// TestPreflightDegenerateWindowNoOp pins the degenerate-window guard: a
// context limit smaller than the wire overhead alone can never fit any
// request (the system prompt and tool definitions ride on every request),
// so the pre-flight check stays out of the way — no forced compaction, no
// summarization calls, history untouched.
func TestPreflightDegenerateWindowNoOp(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	_ = a.ContextStats(context.Background())
	mgr.SetContextLimit(1000)
	if a.wireOverheadTokens() < 1000 {
		t.Fatalf("setup: wire overhead %d not degenerate for limit 1000", a.wireOverheadTokens())
	}
	before := len(a.Messages)

	a.prepareMessages(context.Background(), nil)

	if got := atomic.LoadInt64(&provider.calls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 (degenerate window)", got)
	}
	if len(a.Messages) != before {
		t.Fatalf("messages = %d, want %d (unchanged)", len(a.Messages), before)
	}
}

// TestPreflightProgressGuardSuppressesRetry pins the progress guard: after
// a failed forced-compaction attempt, a retry at the unchanged message
// count is suppressed (it would hit the same failure); growth past that
// count re-arms the attempt.
func TestPreflightProgressGuardSuppressesRetry(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 400)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	_ = a.ContextStats(context.Background())
	setLimitOverEstimate(t, a, mgr, 50)
	provider.fail = true

	a.prepareMessages(context.Background(), nil)
	calls1 := atomic.LoadInt64(&provider.calls)
	if calls1 == 0 {
		t.Fatal("first attempt: no summarization calls")
	}
	// Same message count: the progress guard suppresses the retry.
	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got != calls1 {
		t.Fatalf("same-count retry: %d -> %d summarization calls, want no new calls (progress guard)", calls1, got)
	}
	// Growth past the failed count re-arms the guard.
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_2", Name: "read_file", Args: map[string]interface{}{"path": "main.go"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_2", Content: "package main"})
	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got <= calls1 {
		t.Fatalf("grown-count retry: %d -> %d summarization calls, want a new attempt", calls1, got)
	}
}

// TestPreflightBelowLimitNoOp pins the regression guard: mid-loop,
// comfortably under the context limit, the pre-flight check never fires —
// forced compaction is reserved for the hard window, where the alternative
// is a refused request.
func TestPreflightBelowLimitNoOp(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 400)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	_ = a.ContextStats(context.Background())
	a.statsMu.RLock()
	msgTok := 0
	for _, c := range a.tokenCounts {
		msgTok += c
	}
	a.statsMu.RUnlock()
	mgr.SetContextLimit(a.wireOverheadTokens() + msgTok + 5000)
	before := len(a.Messages)

	view, _ := a.prepareMessages(context.Background(), nil)

	if got := atomic.LoadInt64(&provider.calls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 (below the hard window)", got)
	}
	if len(a.Messages) != before {
		t.Fatalf("messages = %d, want %d (unchanged)", len(a.Messages), before)
	}
	if est := a.outgoingViewEstimate(view); est >= mgr.ContextLimit() {
		t.Fatalf("estimate %d over limit %d in a no-op test", est, mgr.ContextLimit())
	}
}

// TestPreflightStillOverHandsOff pins the still-over hand-off: when the
// forced compaction succeeds (the result strictly shrinks) but the request
// still does not fit the window (the preserved head alone is most of the
// deficit), the view is sent as-is — Phase 0e (last-resort condensation)
// and Phase 3 (provider-refusal recovery) own that state.
func TestPreflightStillOverHandsOff(t *testing.T) {
	a, mgr, provider := newPreflightTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 20)})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 20)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]interface{}{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	_ = a.ContextStats(context.Background())
	// Derive the limit so the pre-compaction estimate is over the window
	// by exactly the savings the compaction makes PLUS a margin: the
	// post-compaction request is still over the window. At keep=0 the
	// middle is the two small messages (the trailing tool group is
	// preserved), so the savings are middleTok - framedTok.
	framedTok := contextmgr.ComputeMessageTokens(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "summary"})
	a.statsMu.RLock()
	middleTok := a.tokenCounts[1] + a.tokenCounts[2]
	msgTok := 0
	for _, c := range a.tokenCounts {
		msgTok += c
	}
	a.statsMu.RUnlock()
	margin := middleTok - framedTok + 30
	if margin <= 0 {
		t.Fatalf("setup: middle %d not large enough for the framed summary %d", middleTok, framedTok)
	}
	mgr.SetContextLimit(a.wireOverheadTokens() + msgTok - margin)

	view, _ := a.prepareMessages(context.Background(), nil)

	if got := atomic.LoadInt64(&provider.calls); got == 0 {
		t.Fatal("no summarization calls: the pre-flight forced compaction did not run")
	}
	// The compaction ran and strictly shrank (5 -> 4: head + summary +
	// the trailing tool group)...
	if len(a.Messages) != 4 {
		t.Fatalf("messages after forced compaction = %d, want 4 (head + summary + tail 2)", len(a.Messages))
	}
	if !contextmgr.IsCompactionSummary(a.Messages[1].Content) {
		t.Fatalf("second message = %q, want a compaction summary", a.Messages[1].Content)
	}
	// ...but the request still does not fit: handed off as-is.
	if est := a.outgoingViewEstimate(view); est < mgr.ContextLimit() {
		t.Fatalf("estimate after forced compaction = %d, want >= limit %d (still-over hand-off)", est, mgr.ContextLimit())
	}
}
