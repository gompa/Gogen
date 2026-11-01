package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestPreflightCondensationErrorDoesNotArmTheGuard pins a one-message
// summary that already fits, followed by a condensation of the larger
// tool that fails. The view is under the window, so the failure does not
// arm the last-resort guard. The overflow path can still condense the tool.
func TestPreflightCondensationErrorDoesNotArmTheGuard(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	provider.failCall = 2
	middle := strings.Repeat("y", 32000)
	toolBody := strings.Repeat("x", 80000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d under the window", est)
	}

	out, err := a.prepareMessages(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, middle) {
		t.Fatal("the one-message middle was left verbatim")
	}
	if !hasContent(a.Messages, toolBody) {
		t.Fatal("the failed condensation replaced the tool")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window", est)
	}
	if a.lastResortGuarded() {
		t.Fatal("the under-window condensation armed the last-resort guard")
	}

	provider.failCall = 0
	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil || !retry {
		t.Fatalf("retry = %v, err = %v", retry, err)
	}
	if hasContent(a.Messages, toolBody) || !toolWasCondensed(a.Messages) {
		t.Fatal("the overflow path left the tool verbatim")
	}
}

// TestPreflightDoesNotResummarizeASingleSummary pins that the pre-flight
// fallback does not run when the first pass already used the single summary.
// The request is the head, so there is no progress summary, and a second
// pass would only re-summarize that summary.
func TestPreflightDoesNotResummarizeASingleSummary(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 300000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 4000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("z", 4000)})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d under the window", est)
	}

	if !a.runForcedSummarization(context.Background(), nil, false) {
		t.Fatal("expected the history to shrink")
	}
	if n := atomic.LoadInt64(&provider.calls); n != 1 {
		t.Fatalf("summarizer calls = %d, want 1", n)
	}
}

// TestPreflightFallbackFailureIsRecorded pins that a failed single-summary
// fallback, after the layout kept the request, is recorded like the forced
// path's other failures. The next preflight does not repeat that summary.
// Condensing the request then fits, and that publish clears the wait the
// failure recorded. The failed single summary stays failed.
func TestPreflightFallbackFailureIsRecorded(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	provider.failAfter = 2
	big := strings.Repeat("x", 300000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))

	if !a.runForcedSummarization(context.Background(), nil, false) {
		t.Fatal("the layout pass should have shrunk the history")
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("the fallback published a summary of the request")
	}
	if backoff, guard := compactState(a); backoff.IsZero() || guard == 0 {
		t.Fatalf("failure not recorded: backoff %v guard %d", backoff, guard)
	}
	if !a.singleSummaryFailed() {
		t.Fatal("the failed single summary was not recorded")
	}
	if a.summarizeRequestAllowed() {
		t.Fatal("the failed single summary is still allowed")
	}
	backoff, _ := compactState(a)
	calls := atomic.LoadInt64(&provider.calls)
	if _, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView()); err != nil {
		t.Fatal(err)
	}
	if got, _ := compactState(a); !got.IsZero() {
		t.Fatalf("backoff = %v, want cleared after the condensation that fit (was %v)", got, backoff)
	}
	if n := atomic.LoadInt64(&provider.calls); n != calls {
		t.Fatalf("summarizer calls = %d, want %d (the failed summary is not repeated)", n, calls)
	}
	if !a.singleSummaryFailed() {
		t.Fatal("the second preflight cleared the failed single summary")
	}
}

// TestPreflightFallbackSignalsCompacting pins that the second summarizer
// pass tells the UI it is compacting, the same way the first pass does.
func TestPreflightFallbackSignalsCompacting(t *testing.T) {
	a, _, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 300000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))
	var n atomic.Int32
	h := &llm.StreamHandlers{OnCompacting: func() { n.Add(1) }}
	if !a.runForcedSummarization(context.Background(), h, false) {
		t.Fatal("expected the history to shrink")
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("OnCompacting fired %d times, want 2", got)
	}
}

// TestPreflightEmptyProgressSummarizesRequest pins the pre-flight fallback
// when the progress region is empty. The layout summarizes what came
// before the request and leaves the request last. The estimate is still
// over the window, so the single summary runs.
func TestPreflightEmptyProgressSummarizesRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 200000)
	seedEmptyProgressRequest(a, big)
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d under the window", est)
	}
	var n atomic.Int32
	h := &llm.StreamHandlers{OnCompacting: func() { n.Add(1) }}

	if !a.runForcedSummarization(context.Background(), h, false) {
		t.Fatal("expected the history to shrink")
	}
	if hasContent(a.Messages, big) {
		t.Fatal("the request is still verbatim")
	}
	if calls := atomic.LoadInt64(&provider.calls); calls < 2 {
		t.Fatalf("summarizer calls = %d, want the layout pass and the single summary", calls)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("OnCompacting fired %d times, want 2", got)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("the fallback was recorded as a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestPreflightOneMessageStillOverSummarizesRequest pins the other side of
// the same shape. The one-message summary is kept, the estimate is still
// over the window, and the single summary then includes the request.
func TestPreflightOneMessageStillOverSummarizesRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 200000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d under the window", est)
	}
	var n atomic.Int32
	h := &llm.StreamHandlers{OnCompacting: func() { n.Add(1) }}

	if !a.runForcedSummarization(context.Background(), h, false) {
		t.Fatal("expected the history to shrink")
	}
	if hasContent(a.Messages, request) {
		t.Fatal("the request is still verbatim")
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("OnCompacting fired %d times, want the layout pass and the single summary", got)
	}
}

// TestPreflightCancelSkipsLastResort pins that a cancel during pre-flight
// returns the cancel. It does not condense, and it does not record a
// compact failure or set either guard.
func TestPreflightCancelSkipsLastResort(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.cancel = cancel
	big := strings.Repeat("x", 300000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	if a.outgoingViewEstimate(view) < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate under the window")
	}

	_, err := a.preflightForcedCompact(ctx, nil, view)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancel", err)
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("last resort condensed the head")
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
	}
	if _, lastResort := guardsState(a); lastResort != 0 {
		t.Fatalf("last-resort guard = %d, want 0", lastResort)
	}
}

// TestPreflightOversizedToolResultKeepsRequest pins a tool result that is
// the part over the window. The layout keeps the request. Condensing the
// tool result fits, so the request stays verbatim.
func TestPreflightOversizedToolResultKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 12000)
	toolBody := strings.Repeat("y", 160000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("w", 4000)})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("w", 4000)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	if a.outgoingViewEstimate(view) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized")
	}
	if hasContent(a.Messages, toolBody) {
		t.Fatal("the tool result is still verbatim")
	}
	found := false
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the tool result was not condensed")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window %d", est, ctxMgr.ContextLimit())
	}
}

// TestPreflightSummarizedRegionsCondenseToolResult pins the steady state
// after a deep-dive compaction. The regions around the request are already
// one summary each, and the next tool result is what exceeds the window.
// The request stays verbatim and the tool result is condensed.
func TestPreflightSummarizedRegionsCondenseToolResult(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 12000)
	toolBody := strings.Repeat("y", 160000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	if a.outgoingViewEstimate(view) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized")
	}
	found := false
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the tool result was not condensed")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window %d", est, ctxMgr.ContextLimit())
	}
}

// TestPreflightTruncatedToolSummarizesRequest pins a tool result that is
// too large to summarize. Condensing it publishes a truncation of about
// the summary input budget, not a 2000-token summary. That truncation
// does not fit beside the request, so the request is summarized and the
// tool result is then condensed.
func TestPreflightTruncatedToolSummarizesRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 50000)
	toolBody := strings.Repeat("y", 200000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	limit := ctxMgr.ContextLimit()
	total := a.outgoingViewEstimate(view)
	if total < limit {
		t.Fatal("test setup: estimate under the window")
	}
	msgs, counts := a.messageCounts()
	idx, c := largestCondensableFrom(msgs, counts, 0)
	if idx < 0 || msgs[idx].Role != "tool" {
		t.Fatal("test setup: the tool result is not the largest condensable message")
	}
	if total-c+lastResortCondenseAllowance >= limit {
		t.Fatalf("test setup: the allowance already refuses (%d - %d + %d >= %d)", total, c, lastResortCondenseAllowance, limit)
	}
	repl, truncated := ctxMgr.CondensedReplacementTokens(msgs[idx])
	if !truncated || total-c+repl < limit {
		t.Fatalf("test setup: truncation %d truncated=%v still fits under %d (total %d, message %d)", repl, truncated, limit, total, c)
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, request) {
		t.Fatal("the request stayed verbatim; the truncation cannot fit beside it")
	}
	for _, m := range a.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("the request was condensed in place")
		}
	}
	if est := a.outgoingViewEstimate(out); est >= limit {
		t.Fatalf("estimate %d still over the window %d", est, limit)
	}
	a.statsMu.RLock()
	guard := a.compactionGuards.emergency
	a.statsMu.RUnlock()
	if guard != 0 {
		t.Fatalf("emergency guard = %d, want 0", guard)
	}
}

// TestPreflightEmptyRegionsSummarizesRequest pins a request that is still
// verbatim because the regions around it were empty. The tool tail is
// smaller than the request, so last resort would condense the request in
// place. The request is summarized instead.
func TestPreflightEmptyRegionsSummarizesRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 200000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "short file"})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	if a.outgoingViewEstimate(view) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if layoutKeptRequest(a.Messages) {
		t.Fatal("test setup: the regions are not empty")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, request) {
		t.Fatal("the request stayed verbatim")
	}
	if !hasContent(a.Messages, "hello") {
		t.Fatal("the head was dropped")
	}
	for _, m := range a.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("the request was condensed in place")
		}
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window %d", est, ctxMgr.ContextLimit())
	}
}

// TestPreflightEmptyRegionsCondenseToolResult pins the other empty-region
// shape. The tool result is the part over the window and a model summary
// of it fits, so the request stays verbatim.
func TestPreflightEmptyRegionsCondenseToolResult(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 80000)
	toolBody := strings.Repeat("x", 94000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	limit := ctxMgr.ContextLimit()
	if a.outgoingViewEstimate(view) < limit {
		t.Fatal("test setup: estimate under the window")
	}
	if layoutKeptRequest(a.Messages) {
		t.Fatal("test setup: the regions are not empty")
	}
	msgs, counts := a.messageCounts()
	idx, _ := largestCondensableFrom(msgs, counts, 0)
	if idx < 0 || msgs[idx].Role != "tool" {
		t.Fatal("test setup: the tool result is not the largest condensable message")
	}
	if _, truncated := ctxMgr.CondensedReplacementTokens(msgs[idx]); truncated {
		t.Fatal("test setup: the tool result is truncated, so this is the other case")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized")
	}
	found := false
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the tool result was not condensed")
	}
	if est := a.outgoingViewEstimate(out); est >= limit {
		t.Fatalf("estimate %d still over the window %d", est, limit)
	}
	a.statsMu.RLock()
	guard := a.compactionGuards.emergency
	a.statsMu.RUnlock()
	if guard != 0 {
		t.Fatalf("emergency guard = %d, want 0", guard)
	}
}

// TestPreflightSmallerMessageKeepsRequest pins that a request is not
// summarized when a smaller message's condensation already fits. The
// request is the largest message. Condensing the tool result lands under
// the window, so the request stays verbatim.
func TestPreflightSmallerMessageKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	toolBody := strings.Repeat("x", 64000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	limit := ctxMgr.ContextLimit()
	total := a.outgoingViewEstimate(view)
	if total < limit {
		t.Fatal("test setup: estimate under the window")
	}
	msgs, counts := a.messageCounts()
	largest, _ := largestCondensableFrom(msgs, counts, 0)
	if largest < 0 || msgs[largest].Content != request {
		t.Fatal("test setup: the request is not the largest condensable message")
	}
	var toolIdx, toolTok int
	for i, m := range msgs {
		if m.Role == "tool" {
			toolIdx, toolTok = i, counts[i]
		}
	}
	if _, truncated := ctxMgr.CondensedReplacementTokens(msgs[toolIdx]); truncated {
		t.Fatal("test setup: the tool result is truncated, so this is the other case")
	}
	if total-toolTok+lastResortCondenseAllowance >= limit {
		t.Fatalf("test setup: condensing the tool does not fit (%d - %d + %d >= %d)", total, toolTok, lastResortCondenseAllowance, limit)
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized")
	}
	found := false
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the smaller tool result was not condensed")
	}
	if est := a.outgoingViewEstimate(out); est >= limit {
		t.Fatalf("estimate %d still over the window %d", est, limit)
	}
}

// TestPreflightCancelDuringCondense pins that a cancel while last resort
// is summarizing a message is returned, and is not stored as a failure.
// The request stays verbatim.
func TestPreflightCancelDuringCondense(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.cancel = cancel
	provider.cancelAfter = 1
	request := "keep-this-request\n" + strings.Repeat("x", 80000)
	toolBody := strings.Repeat("x", 94000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	if a.outgoingViewEstimate(view) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}

	_, err := a.preflightForcedCompact(ctx, nil, view)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancel", err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized after the cancel")
	}
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("the tool result was condensed after the cancel")
		}
	}
	if _, lastResort := guardsState(a); lastResort != 0 {
		t.Fatalf("last-resort guard = %d, want 0", lastResort)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestPreflightOversizedSummaryKeepsRequest pins a condensation whose
// planned size fits and whose summary comes back too big. The request
// stays verbatim. The emergency guard is set so the next round does not
// summarize again.
func TestPreflightOversizedSummaryKeepsRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.hugeFirst = true
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	toolBody := strings.Repeat("x", 64000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	limit := ctxMgr.ContextLimit()
	if a.outgoingViewEstimate(view) < limit {
		t.Fatal("test setup: estimate under the window")
	}
	msgs, counts := a.messageCounts()
	var toolIdx, toolTok int
	for i, m := range msgs {
		if m.Role == "tool" {
			toolIdx, toolTok = i, counts[i]
		}
	}
	total := a.outgoingViewEstimate(view)
	if _, truncated := ctxMgr.CondensedReplacementTokens(msgs[toolIdx]); truncated {
		t.Fatal("test setup: the tool result is truncated")
	}
	if total-toolTok+lastResortCondenseAllowance >= limit {
		t.Fatal("test setup: the planned condensation does not fit")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized")
	}
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("the oversized summary was published")
		}
	}
	if est := a.outgoingViewEstimate(out); est < limit {
		t.Fatalf("estimate %d is under the window; the miss did not happen", est)
	}
	if _, guard := compactState(a); guard == 0 {
		t.Fatal("a view still over the window left the emergency guard clear")
	}
}

// TestPreflightStillOverSetsEmergencyGuard pins a published summary that
// leaves the view over the window when the request is the head. The
// emergency guard is set again after the publish cleared it.
func TestPreflightStillOverSetsEmergencyGuard(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	head := strings.Repeat("x", 200000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("w", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: strings.Repeat("y", 200000)})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, strings.Repeat("w", 8000)) {
		t.Fatal("test setup: the compaction did not publish")
	}
	if est := a.outgoingViewEstimate(out); est < ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d is under the window", est)
	}
	if _, guard := compactState(a); guard == 0 {
		t.Fatal("a view still over the window left the emergency guard clear")
	}
}

// TestPreflightOversizedRequestSummaryStays pins an attempt whose planned
// replacement fits and whose summary comes back too big, when that message
// is the request. The request stays verbatim. Forced summarization is
// already guarded, so this pass is last resort and not the single summary.
func TestPreflightOversizedRequestSummaryStays(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.hugeFirst = true
	request := "keep-this-request\n" + strings.Repeat("x", 70000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + strings.Repeat("x", 90000)})
	_ = a.ContextStats(context.Background())
	view := a.rebuildView()
	limit := ctxMgr.ContextLimit()
	total := a.outgoingViewEstimate(view)
	if total < limit {
		t.Fatal("test setup: estimate under the window")
	}
	msgs, counts := a.messageCounts()
	idx, ok := a.condensableChoice(total, msgs, counts, limit)
	if !ok || msgs[idx].Content != request {
		t.Fatal("test setup: last resort would not condense the request")
	}
	if _, truncated := ctxMgr.CondensedReplacementTokens(msgs[idx]); truncated {
		t.Fatal("test setup: the request is truncated, so the model is not called")
	}
	a.statsMu.Lock()
	a.compactionGuards.emergency = len(a.Messages)
	a.statsMu.Unlock()

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the oversized summary of the request was followed by the single summary")
	}
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("the oversized summary was published")
		}
	}
	if est := a.outgoingViewEstimate(out); est < limit {
		t.Fatalf("estimate %d is under the window; the miss did not happen", est)
	}
	if _, lastResort := guardsState(a); lastResort != 0 {
		t.Fatalf("last-resort guard = %d, want 0", lastResort)
	}
}
