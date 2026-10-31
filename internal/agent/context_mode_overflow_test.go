package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestDeepDiveOverflowSummarizesDominantRequest pins the overflow path when
// the estimate already says the request is over the window: the provider
// refused, deep dive summarizes the request in that pass, and the one retry
// is spent on a history that fits.
func TestDeepDiveOverflowSummarizesDominantRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 300000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if !retry || err != nil {
		t.Fatalf("retry = %v, err = %v; want a retry", retry, err)
	}
	if hasContent(a.Messages, big) {
		t.Fatal("the oversized request is still verbatim")
	}
	for _, m := range a.Messages {
		if contextmgr.IsCondensedMessage(m.Content) || strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) {
			t.Fatalf("want the single summary, got a deep-dive or condensed message: %.60q", m.Content)
		}
	}
	if total, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); total >= limit {
		t.Fatalf("view after recovery = %d tokens, window %d", total, limit)
	}
}

// TestOverflowHugeHeadRetriesBeforeLastResort pins the overflow path in
// default mode. A huge first message with history after it is shrunk by
// the forced compaction and the estimate is still over the window. The
// retry is not spent on that shrink. Last resort condenses the head,
// because the truncation fits, and the retry is that result.
func TestOverflowHugeHeadRetriesBeforeLastResort(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 300000)
	middle := strings.Repeat("y", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	_ = a.ContextStats(context.Background())

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil || !retry {
		t.Fatalf("retry = %v, err = %v", retry, err)
	}
	if hasContent(a.Messages, big) {
		t.Fatal("last resort left the head verbatim")
	}
	if !contextmgr.IsCondensedMessage(a.Messages[0].Content) {
		t.Fatal("the head was not condensed")
	}
	if hasContent(a.Messages, middle) {
		t.Fatal("the compaction did not change the history")
	}
	if total, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); total >= limit {
		t.Fatalf("retried at %d tokens, window %d", total, limit)
	}
}

// TestDeepDiveOverflowRefusalUnderEstimate pins the refusal the run loop
// actually produces. Pre-flight compacts only when the estimate is already
// over the window, so a provider refusal arrives with the estimate under
// it. The layout around the request fits, so the request stays verbatim
// and that result is what is sent again.
func TestDeepDiveOverflowRefusalUnderEstimate(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 60000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))
	if est, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); est >= limit {
		t.Fatalf("test setup: estimate %d is not under the window %d", est, limit)
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if !retry || err != nil {
		t.Fatalf("retry = %v, err = %v; want a retry", retry, err)
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("a fitting layout summarized the request")
	}
	if !layoutKeptRequest(a.Messages) {
		t.Fatal("the deep-dive layout did not keep the request")
	}
	if n := atomic.LoadInt64(&provider.calls); n == 0 {
		t.Fatal("the layout did not run")
	}
	if total, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); total >= limit {
		t.Fatalf("view after recovery = %d tokens, window %d", total, limit)
	}
}

// TestOverflowSingleSummaryFailureIsRemembered pins a single summary that
// fails on a provider refusal. The next refusal does not run it again and
// does not double the wait. The emergency guard is not that check.
func TestOverflowSingleSummaryFailureIsRemembered(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	s := ctxMgr.SettingsSnapshot()
	s.CompactLastResort = "error"
	ctxMgr.UpdateSettings(s)
	provider.failAfter = 2
	big := strings.Repeat("x", 300000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d is under the window", est)
	}

	_, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err == nil {
		t.Fatal("the failed summary retried")
	}
	if !a.singleSummaryFailed() {
		t.Fatal("the failed single summary was not recorded")
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("the failed summary replaced the request")
	}
	first := atomic.LoadInt64(&provider.calls)
	if first < 2 {
		t.Fatalf("summarizer calls = %d, want the layout and the failed summary", first)
	}
	delay := a.compactBackoffDelay

	_, err = a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err == nil {
		t.Fatal("the second refusal retried the failed summary")
	}
	if calls := atomic.LoadInt64(&provider.calls); calls != first {
		t.Fatalf("summarizer calls = %d, want %d", calls, first)
	}
	if a.compactBackoffDelay != delay || a.compactBackoffDelay == 60*time.Second {
		t.Fatalf("backoff delay = %s, want it to stay %s", a.compactBackoffDelay, delay)
	}
}

// TestOverflowUnderWindowCondensesTheHead pins a provider refusal whose
// largest message is the head. Deep dive condenses the head. A smaller
// tool result stays, and the retry is that condensation.
func TestOverflowUnderWindowCondensesTheHead(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	head := strings.Repeat("x", 70000)
	toolBody := strings.Repeat("y", 30000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d is over the window", est)
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil || !retry {
		t.Fatalf("retry = %v, err = %v", retry, err)
	}
	if hasContent(a.Messages, head) || !hasContent(a.Messages, toolBody) || !hasContent(a.Messages, "note") {
		t.Fatal("the head stayed verbatim or the smaller tool was replaced")
	}
	condensedHead := false
	for _, m := range a.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			condensedHead = true
		}
	}
	if !condensedHead {
		t.Fatal("the head was summarized instead of condensed")
	}
}

// TestOverflowCondensationErrorDoesNotSummarizeTheMiddle pins a provider
// refusal whose head is larger than the middle. The condensation of that
// head fails. The smaller middle is not summarized and the retry is not
// spent. The failure is recorded, so the next refusal does not try it again.
func TestOverflowCondensationErrorDoesNotSummarizeTheMiddle(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.failCall = 1
	head := strings.Repeat("x", 70000)
	toolBody := strings.Repeat("y", 30000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d is over the window", est)
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err == nil || retry {
		t.Fatalf("retry = %v, err = %v, want the refusal back", retry, err)
	}
	if calls := atomic.LoadInt64(&provider.calls); calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1 (the failed condensation)", calls)
	}
	if !hasContent(a.Messages, head) || !hasContent(a.Messages, toolBody) || !hasContent(a.Messages, "note") {
		t.Fatal("the failed condensation replaced the head or summarized the middle")
	}
	if got := a.overflowRetries.Load(); got != 0 {
		t.Fatalf("overflow retries = %d, want 0", got)
	}
	if !a.lastResortGuarded() {
		t.Fatal("the failed overflow condensation was not recorded")
	}
}

// TestOverflowUnderWindowCondensesThePreservedTail pins a provider
// refusal that arrives with the estimate under the window. Compaction
// would leave the large message verbatim. Deep dive condenses it and
// keeps the request, and the retry is that condensation. A larger
// request is still summarized, which TestDeepDiveOverflowRefusalUnderEstimate
// pins. Context mode off condenses that tool on the same path.
func TestOverflowUnderWindowCondensesThePreservedTail(t *testing.T) {
	t.Run("tool result", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		toolBody := strings.Repeat("x", 80000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: "the request"})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("test setup: estimate %d is over the window", est)
		}

		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if err != nil || !retry {
			t.Fatalf("retry = %v, err = %v", retry, err)
		}
		if !hasContent(a.Messages, "the request") || !hasContent(a.Messages, "hello") || hasContent(a.Messages, toolBody) {
			t.Fatal("the refusal summarized the request or left the tool verbatim")
		}
		if !toolWasCondensed(a.Messages) {
			t.Fatal("the preserved tool was not condensed")
		}
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("estimate %d still over the window", est)
		}
		retry, err = a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if retry || err == nil {
			t.Fatalf("second refusal retry = %v, err = %v", retry, err)
		}
		if !hasContent(a.Messages, "the request") {
			t.Fatal("the second refusal summarized the request")
		}
	})

	t.Run("pinned request", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		request := "keep-this-request\n" + strings.Repeat("x", 80000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: request})
		_ = a.ContextStats(context.Background())
		pm := NewPinManager()
		pm.pinned[1] = struct{}{}
		a.PinManager = pm
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("test setup: estimate %d is over the window", est)
		}

		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if err != nil || !retry {
			t.Fatalf("retry = %v, err = %v", retry, err)
		}
		if hasContent(a.Messages, request) || a.Messages[0].Content != "hello" {
			t.Fatal("the pinned request stayed verbatim or the head changed")
		}
		if !pinnedCondensed(pm, a.Messages) {
			t.Fatal("the pinned request was summarized instead of condensed")
		}
	})

	t.Run("context mode off condenses the tool", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		toolBody := strings.Repeat("x", 80000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: "the request"})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("test setup: estimate %d is over the window", est)
		}

		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if err != nil || !retry {
			t.Fatalf("retry = %v, err = %v", retry, err)
		}
		if !hasContent(a.Messages, "the request") || !hasContent(a.Messages, "hello") || hasContent(a.Messages, toolBody) {
			t.Fatal("context mode off summarized the request or left the tool verbatim")
		}
		if !toolWasCondensed(a.Messages) {
			t.Fatal("context mode off did not condense the tool")
		}
		assertCompactionRecovered(t, a)
	})
}

// TestOverflowCancelSkipsLastResort pins that a cancel during the forced
// pass returns the cancel and does not condense. Last resort would
// otherwise run on the cancelled context and the turn would still retry.
func TestOverflowCancelSkipsLastResort(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.cancel = cancel
	big := strings.Repeat("x", 300000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	_ = a.ContextStats(context.Background())

	retry, err := a.handleOverflowError(ctx, nil, llm.ErrContextWindowExceeded)
	if retry || !errors.Is(err, context.Canceled) {
		t.Fatalf("retry = %v, err = %v; want the cancel", retry, err)
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("last resort condensed the head")
	}
	if _, lastResort := guardsState(a); lastResort != 0 {
		t.Fatalf("last-resort guard = %d, want 0", lastResort)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestOverflowShrinkStillOverErrorMode pins a shrink in
// compact_last_resort=error. The history changed and the estimate is still
// over the window. Error mode does not condense and does not spend the
// retry. The diagnostic is returned, and the emergency guard is set.
func TestOverflowShrinkStillOverErrorMode(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	s := ctxMgr.SettingsSnapshot()
	s.CompactLastResort = "error"
	ctxMgr.UpdateSettings(s)
	big := strings.Repeat("x", 300000)
	middle := strings.Repeat("y", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	_ = a.ContextStats(context.Background())

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if retry || err == nil {
		t.Fatalf("retry = %v, err = %v; want the diagnostic", retry, err)
	}
	if !strings.Contains(err.Error(), "shorten it") {
		t.Fatalf("err = %v, want the last-resort diagnostic", err)
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("error mode condensed the head")
	}
	if hasContent(a.Messages, middle) {
		t.Fatal("the compaction did not change the history")
	}
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d is under the window", est)
	}
	if got := a.overflowRetries.Load(); got != 0 {
		t.Fatalf("overflow retries = %d, want 0", got)
	}
	if backoff, guard := compactState(a); backoff.IsZero() || guard == 0 {
		t.Fatalf("backoff %v guard %d, want both set", backoff, guard)
	}
}

// TestOverflowShrinkStillOverRetries pins a forced compaction that changes
// the history while the estimate stays over the window. The retry is not
// spent on that shrink. Last resort runs on it. The retry happens when
// the published replacement measures under the window. A replacement that
// stays over ends the turn with the shrunk history and no retry.
func TestOverflowShrinkStillOverRetries(t *testing.T) {
	t.Run("replacement fits", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		head := strings.Repeat("x", 90000)
		middle := strings.Repeat("w", 8000)
		toolBody := strings.Repeat("x", 80000)
		a.appendMessage(llm.Message{Role: "user", Content: head})
		a.appendMessage(llm.Message{Role: "assistant", Content: middle})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
			t.Fatalf("test setup: estimate %d is under the window", est)
		}

		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if err != nil || !retry {
			t.Fatalf("retry = %v, err = %v", retry, err)
		}
		if hasContent(a.Messages, middle) {
			t.Fatal("the compaction did not change the history")
		}
		if hasContent(a.Messages, head) {
			t.Fatal("last resort left the oversized head verbatim")
		}
		if !hasContent(a.Messages, toolBody) {
			t.Fatal("last resort condensed the smaller tool")
		}
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("retried at estimate %d, still over the window", est)
		}
		if got := a.overflowRetries.Load(); got != 1 {
			t.Fatalf("overflow retries = %d, want 1", got)
		}
	})
	t.Run("replacement stays over", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		head := strings.Repeat("x", 300000)
		middle := strings.Repeat("w", 8000)
		toolBody := strings.Repeat("y", 300000)
		a.appendMessage(llm.Message{Role: "user", Content: head})
		a.appendMessage(llm.Message{Role: "assistant", Content: middle})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
			t.Fatalf("test setup: estimate %d is under the window", est)
		}

		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if retry || err == nil {
			t.Fatalf("retry = %v, err = %v; want the turn to end", retry, err)
		}
		if !errors.Is(err, llm.ErrContextWindowExceeded) {
			t.Fatalf("terminal error lost the original refusal: %v", err)
		}
		if hasContent(a.Messages, middle) {
			t.Fatal("the compaction did not change the history")
		}
		if !hasContent(a.Messages, head) || !hasContent(a.Messages, toolBody) {
			t.Fatal("a replacement that stays over was published")
		}
		if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
			t.Fatalf("estimate %d is under the window", est)
		}
		if got := a.overflowRetries.Load(); got != 0 {
			t.Fatalf("overflow retries = %d, want 0", got)
		}
	})
}

// TestOverflowShrinkCancelSkipsLastResort pins a cancel during last
// resort after a shrink that is still over the window. The cancel is
// returned. The tool stays. The retry is not spent, and the cancel is
// not stored as a failure.
func TestOverflowShrinkCancelSkipsLastResort(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.cancel = cancel
	provider.cancelAfter = 2
	head := strings.Repeat("x", 90000)
	middle := strings.Repeat("w", 8000)
	toolBody := strings.Repeat("x", 80000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())

	retry, err := a.handleOverflowError(ctx, nil, llm.ErrContextWindowExceeded)
	if retry || !errors.Is(err, context.Canceled) {
		t.Fatalf("retry = %v, err = %v; want the cancel", retry, err)
	}
	if !hasContent(a.Messages, toolBody) || !hasContent(a.Messages, head) {
		t.Fatal("the cancel condensed a message")
	}
	if hasContent(a.Messages, middle) {
		t.Fatal("the shrink did not run before last resort")
	}
	if got := a.overflowRetries.Load(); got != 0 {
		t.Fatalf("overflow retries = %d, want 0", got)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestOverflowAfterOversizedSummaryKeepsRequest pins a provider refusal
// after a condensation summary came back too big. Overflow must not
// summarize the request that preflight left verbatim.
func TestOverflowAfterOversizedSummaryKeepsRequest(t *testing.T) {
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
	if a.outgoingViewEstimate(view) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if _, err := a.preflightForcedCompact(context.Background(), nil, view); err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, toolBody) {
		t.Fatal("test setup: preflight did not leave the request and the tool")
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil && !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("overflow summarized the request")
	}
	if retry {
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("retried at estimate %d, still over the window", est)
		}
	}
}

// TestOverflowGuardedCondensationSummarizesRequest pins the other side of
// that refusal. Condensation already failed at this message count, so the
// provider refusal summarizes the request even though the tool's
// replacement would fit.
func TestOverflowGuardedCondensationSummarizesRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: strings.Repeat("x", 64000)})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	a.noteProgressFailure(&a.compactionGuards.lastResort)
	if !a.lastResortGuarded() {
		t.Fatal("test setup: condensation is not guarded")
	}
	if a.leaveForCondensation() {
		t.Fatal("test setup: a guarded condensation still defers the request")
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil && !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&provider.calls) == 0 {
		t.Fatal("overflow did not summarize")
	}
	if hasContent(a.Messages, request) {
		t.Fatal("a guarded condensation left the request verbatim")
	}
	if retry {
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("retried at estimate %d, still over the window", est)
		}
	}
}
