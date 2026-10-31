package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

func hasContent(msgs []llm.Message, content string) bool {
	for _, m := range msgs {
		if m.Content == content {
			return true
		}
	}
	return false
}

// seedDominantRequest builds a deep-dive history around a request that is
// most of the window: head, session summary, the request, a progress
// summary, and work after it that the layout around the request can
// summarize, so that layout succeeds and still leaves the request verbatim.
func seedDominantRequest(a *Agent, request string, after ...string) {
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	for _, content := range after {
		a.appendMessage(llm.Message{Role: "assistant", Content: content})
	}
	_ = a.ContextStats(context.Background())
}

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

// TestEmergencyDeepDiveDominantRequest pins the emergency tier's fit
// fallback: the layout around a request that is most of the window
// summarizes the work at every keep and stays over the hard limit, so the
// tier summarizes the request too instead of recording a failure.
func TestEmergencyDeepDiveDominantRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 120000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if hasContent(a.Messages, big) {
		t.Fatal("the dominant request is still verbatim")
	}
	if total := a.compactionTokenTotal(); total >= ctxMgr.HardLimit() {
		t.Fatalf("total after the emergency tier = %d, hard limit %d", total, ctxMgr.HardLimit())
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestCompactHistoryNothingToCompact pins that /compact (CompactHistory,
// shared by the TUI and the web UI) does not report success when the
// compaction changed nothing: in deep dive, the regions around the request
// already are one summary each.
func TestCompactHistoryNothingToCompact(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedDominantRequest(a, "the request", "tail one", "tail two")
	before := append([]llm.Message(nil), a.Messages...)

	err := a.CompactHistory(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing to compact") {
		t.Fatalf("err = %v, want nothing to compact", err)
	}
	if len(a.Messages) != len(before) || provider.calls != 0 {
		t.Fatalf("messages %d -> %d, %d summarizer calls", len(before), len(a.Messages), provider.calls)
	}
}

// TestNormalTierKeepsLargeRequest pins that the fit fallback belongs to the
// emergency tier only: below the hard limit, the normal tier keeps a large
// request verbatim (the point of deep dive) even when it stays over the
// budget.
func TestNormalTierKeepsLargeRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 80000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000), "tail1", "tail2")
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()
	if total := a.compactionTokenTotal(); total < budget || total >= hard {
		t.Fatalf("test setup: total %d not in [budget %d, hard limit %d)", total, budget, hard)
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, false)
	if !hasContent(a.Messages, big) {
		t.Fatal("the normal tier summarized the request")
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

// TestSameCountSummaryStillCondensesTheLargerTool pins context mode off.
// A one-message summary is published and drops the estimate under the
// window. The trailing tool was larger than that message, so it is
// condensed before the view is sent. A smaller pinned tool is left,
// which TestPreflightOneMessageSummaryIsKept pins.
func TestSameCountSummaryStillCondensesTheLargerTool(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	middle := strings.Repeat("y", 32000)
	toolBody := strings.Repeat("x", 80000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	_, counts := a.messageCounts()
	if len(counts) < 4 || counts[3] <= counts[1] {
		t.Fatalf("test setup: tool is not larger than the middle: %v", counts)
	}
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
	if hasContent(a.Messages, toolBody) || !toolWasCondensed(a.Messages) {
		t.Fatal("the larger tool was left verbatim")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window", est)
	}
}

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

// TestGuardedLastResortStillSummarizesTheMiddle pins a last-resort guard
// that was set for another reason. No preserved message is larger than
// the middle, so the guard does not block that summary.
func TestGuardedLastResortStillSummarizesTheMiddle(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	middle := strings.Repeat("w", 4000)
	toolBody := strings.Repeat("x", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	_, counts := a.messageCounts()
	if len(counts) < 4 || counts[3] >= counts[1] {
		t.Fatalf("test setup: tool is not smaller than the middle: %v", counts)
	}
	if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d is over the window", est)
	}
	a.noteProgressFailure(&a.compactionGuards.lastResort)

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil || !retry {
		t.Fatalf("retry = %v, err = %v", retry, err)
	}
	if atomic.LoadInt64(&provider.calls) == 0 {
		t.Fatal("the guard blocked the summary of the middle")
	}
	if hasContent(a.Messages, middle) || !hasContent(a.Messages, toolBody) {
		t.Fatal("the middle stayed or the smaller tool was replaced")
	}
}

// TestPublishedCondensationClearsTheBackoff pins a single-summary failure
// that recorded a wait, followed by a condensation that fits. The wait is
// cleared. The failed single summary stays failed.
func TestPublishedCondensationClearsTheBackoff(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 200000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
	pm := NewPinManager()
	pm.pinned[1] = struct{}{}
	a.PinManager = pm
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	a.noteCompactFailure(errors.New("single summary failed"))
	a.noteProgressFailure(&a.compactionGuards.singleSummary)
	if !a.singleSummaryFailed() || a.compactBackoffDelay == 0 {
		t.Fatal("test setup: the failed summary was not recorded")
	}

	out, err := a.prepareMessages(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, request) {
		t.Fatal("the pinned request was left verbatim")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window", est)
	}
	if !a.singleSummaryFailed() {
		t.Fatal("the published condensation cleared the failed single summary")
	}
	if backoff, _ := compactState(a); !backoff.IsZero() || a.compactBackoffDelay != 0 {
		t.Fatalf("backoff %v delay %s, want both clear", backoff, a.compactBackoffDelay)
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

// TestNormalTierOverHardLimitKeepsRequest pins the emergency gate on its
// own. The total is at or above the hard limit, but the tier is the normal
// one, so the request stays verbatim. Removing the emergency condition
// would summarize it.
func TestNormalTierOverHardLimitKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 120000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, false)
	if !hasContent(a.Messages, big) {
		t.Fatal("the normal tier summarized the request")
	}
	if total := a.compactionTokenTotal(); total < ctxMgr.CompactBudget() {
		t.Fatalf("test setup: compaction reached the budget (%d) before the fallback check", total)
	}
	if _, guard := compactState(a); guard != 0 {
		t.Fatalf("normal-tier give-up set the emergency guard to %d", guard)
	}
}

// TestEmergencyBelowHardLimitKeepsRequest pins the hard-limit gate on its
// own. The tier is emergency, but the total stays under the hard limit, so
// the request stays verbatim and no failure is recorded. The budget belongs
// to the normal tier. Removing the hard-limit stop would arm the backoff.
func TestEmergencyBelowHardLimitKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 100000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if !hasContent(a.Messages, big) {
		t.Fatal("the emergency tier summarized the request below the hard limit")
	}
	if total := a.compactionTokenTotal(); total < budget || total >= hard {
		t.Fatalf("test setup: total %d left [budget %d, hard limit %d) before the fallback check", total, budget, hard)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure under the hard limit: backoff %v guard %d", backoff, guard)
	}
}

// TestEmergencyFallbackSkipsSingleSummary pins that the emergency fallback
// does not run when the history is already the single summary. The head is
// the request, so there is no progress summary after it. Another pass would
// fail the minimum-size guard and replace this loop's diagnostic.
func TestEmergencyFallbackSkipsSingleSummary(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	big := strings.Repeat("x", 120000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("z", 8000)})
	_ = a.ContextStats(context.Background())

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 0, true)
	logged := buf.String()
	if !strings.Contains(logged, "still over budget") {
		t.Fatalf("log %q, want the loop's own failure", logged)
	}
	if strings.Contains(logged, "not enough history") {
		t.Fatalf("the fallback replaced the diagnostic: %s", logged)
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("the head was summarized")
	}
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit, so the fallback was not reached", total)
	}
	if n := atomic.LoadInt64(&provider.calls); n != 1 {
		t.Fatalf("summarizer calls = %d, want the one layout pass", n)
	}
	if _, guard := compactState(a); guard == 0 {
		t.Fatal("emergency give-up left the guard clear")
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

// TestLayoutKeptRequestRequiresProgressAfterLatestRequest pins the
// predicate: a progress summary counts only when it follows the latest
// request.
func TestLayoutKeptRequestRequiresProgressAfterLatestRequest(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "old"},
		{Role: "user", Content: "later"},
	}
	if layoutKeptRequest(msgs) {
		t.Fatal("a progress summary before the latest request is not the layout")
	}
	msgs = append(msgs, llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "now"})
	if !layoutKeptRequest(msgs) {
		t.Fatal("a progress summary after the latest request is the layout")
	}
}

// seedEmptyProgressRequest builds a deep-dive history whose progress region
// is empty. The work before the request is large enough to summarize, so
// the layout shrinks and still leaves the request verbatim.
func seedEmptyProgressRequest(a *Agent, request string) {
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("z", 8000)})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
}

// TestEmergencySmallMiddleReachesLastRequest pins that a minimum-size
// refusal does not end the emergency sequence. The request is the last
// message, so the first keep leaves it in the tail and the middle is too
// small to summarize. The next keep includes the request.
func TestEmergencySmallMiddleReachesLastRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	big := strings.Repeat("x", 150000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	a.appendMessage(llm.Message{Role: "user", Content: big})
	_ = a.ContextStats(context.Background())
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if hasContent(a.Messages, big) {
		t.Fatal("the last-message request is still verbatim")
	}
	if total := a.compactionTokenTotal(); total >= ctxMgr.HardLimit() {
		t.Fatalf("total %d still at the hard limit %d", total, ctxMgr.HardLimit())
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("a too-small middle was recorded as a failure: backoff %v guard %d", backoff, guard)
	}
	if n := atomic.LoadInt64(&provider.calls); n == 0 {
		t.Fatal("summarizer calls = 0, want the keep that includes the request")
	}
}

// TestDefaultModeSmallMiddleStops pins the pre-deep-dive answer. A middle
// under the minimum size arms the backoff and the emergency guard. The
// large message just sent stays verbatim for condensation.
func TestDefaultModeSmallMiddleStops(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	big := "keep-this-request\n" + strings.Repeat("x", 150000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	a.appendMessage(llm.Message{Role: "user", Content: big})
	_ = a.ContextStats(context.Background())
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if !hasContent(a.Messages, big) {
		t.Fatal("default mode summarized the message just sent")
	}
	if atomic.LoadInt64(&provider.calls) != 0 {
		t.Fatal("default mode summarized past a too-small middle")
	}
	if backoff, guard := compactState(a); backoff.IsZero() || guard == 0 {
		t.Fatalf("backoff %v guard %d, want both set", backoff, guard)
	}
}

// TestNormalSmallMiddleTriesSmallerKeep pins the same minimum-size skip on
// the normal tier. The first keep's middle is too small, and the next keep
// summarizes what that keep had left in the tail. Recording the refusal
// would back off with the history unchanged.
func TestNormalSmallMiddleTriesSmallerKeep(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	tail := strings.Repeat("y", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	a.appendMessage(llm.Message{Role: "assistant", Content: tail})
	_ = a.ContextStats(context.Background())

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, false)
	if hasContent(a.Messages, tail) {
		t.Fatal("the smaller keep left the tail verbatim")
	}
	if backoff, _ := compactState(a); !backoff.IsZero() {
		t.Fatalf("a too-small middle was recorded as a failure: backoff %v", backoff)
	}
	if n := atomic.LoadInt64(&provider.calls); n == 0 {
		t.Fatal("summarizer calls = 0, want the smaller keep")
	}
}

// TestEmergencySummarizerErrorStopsTheSequence pins that a summarizer
// failure still ends the shrinking-tail sequence. The minimum-size skip
// does not apply to it, so later keeps do not each call the provider.
func TestEmergencySummarizerErrorStopsTheSequence(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, true)
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("z", 8000)})
	a.appendMessage(llm.Message{Role: "user", Content: "tail"})
	_ = a.ContextStats(context.Background())

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, true)
	if n := atomic.LoadInt64(&provider.calls); n != 2 {
		t.Fatalf("summarizer calls = %d, want the one failed pass", n)
	}
	if _, guard := compactState(a); guard == 0 {
		t.Fatal("the summarizer failure was not recorded")
	}
	if !hasContent(a.Messages, strings.Repeat("y", 8000)) {
		t.Fatal("a later keep summarized the history after the failure")
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

// TestEmergencyEmptyProgressSummarizesRequest pins the same empty progress
// region on the emergency tier. The layout shrinks the history and the
// total stays at or above the hard limit, so the request is summarized.
func TestEmergencyEmptyProgressSummarizesRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 200000)
	seedEmptyProgressRequest(a, big)
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 0, true)
	if hasContent(a.Messages, big) {
		t.Fatal("the request is still verbatim")
	}
	if total := a.compactionTokenTotal(); total >= ctxMgr.HardLimit() {
		t.Fatalf("total %d still at the hard limit %d", total, ctxMgr.HardLimit())
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure after the request was summarized: backoff %v guard %d", backoff, guard)
	}
	if n := atomic.LoadInt64(&provider.calls); n < 2 {
		t.Fatalf("summarizer calls = %d, want the layout pass and the single summary", n)
	}
}

// TestLayoutKeptRequestIncludesEmptyProgress pins the empty progress
// region: one session summary between the head and the request. Other
// messages in that gap are a different history.
func TestLayoutKeptRequestIncludesEmptyProgress(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"},
		{Role: "user", Content: "the request"},
	}
	if !layoutKeptRequest(msgs) {
		t.Fatal("an empty progress region keeps the request after the session summary")
	}
	withTail := append(append([]llm.Message{}, msgs...), llm.Message{Role: "assistant", Content: "tail"})
	if !layoutKeptRequest(withTail) {
		t.Fatal("a tail after that request is still the layout")
	}
	mixed := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"},
		{Role: "assistant", Content: "more"},
		{Role: "user", Content: "the request"},
	}
	if layoutKeptRequest(mixed) {
		t.Fatal("other messages between the session summary and the request are a different history")
	}
}

// TestEmergencyOneMessageSummaryKeepsRequest pins a progress region of one
// message. The summary does not drop the count, and the tokens are already
// under the hard limit. The request stays verbatim.
func TestEmergencyOneMessageSummaryKeepsRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 90000)
	reply := strings.Repeat("y", 40000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: reply})
	_ = a.ContextStats(context.Background())
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 0, true)
	if !hasContent(a.Messages, request) {
		t.Fatal("the one-message summary was discarded and the request was summarized")
	}
	if hasContent(a.Messages, reply) {
		t.Fatal("the one-message region is still verbatim")
	}
	if !layoutKeptRequest(a.Messages) {
		t.Fatal("the progress summary is missing")
	}
	if total := a.compactionTokenTotal(); total >= ctxMgr.HardLimit() {
		t.Fatalf("total %d still at the hard limit %d", total, ctxMgr.HardLimit())
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure under the hard limit: backoff %v guard %d", backoff, guard)
	}
	if n := atomic.LoadInt64(&provider.calls); n != 1 {
		t.Fatalf("summarizer calls = %d, want the one progress summary", n)
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

// TestEmergencyOversizedTailKeepsRequest pins the keep that cannot
// summarize the tail. The progress region is under the minimum, so this
// keep writes nothing. The next keep summarizes the tail and leaves the
// request verbatim. Summarizing the request on this keep would keep the
// tail and stop, because the tail alone is under the hard limit.
func TestEmergencyOversizedTailKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := strings.Repeat("x", 30000)
	tail := strings.Repeat("y", 90000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("n", 800)})
	a.appendMessage(llm.Message{Role: "assistant", Content: tail})
	_ = a.ContextStats(context.Background())
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized on the keep that left the tail verbatim")
	}
	if hasContent(a.Messages, tail) {
		t.Fatal("the oversized tail is still verbatim")
	}
	if total := a.compactionTokenTotal(); total >= ctxMgr.HardLimit() {
		t.Fatalf("total %d still at the hard limit %d", total, ctxMgr.HardLimit())
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure after the tail was summarized: backoff %v guard %d", backoff, guard)
	}
}

// TestDeepDiveUnchangedKeepKeepsTheBaseline pins a provider count over the
// budget whose local estimate is under it. The first keep writes nothing.
// Publishing that result would drop the provider count and stop, with the
// recent work still verbatim.
func TestDeepDiveUnchangedKeepKeepsTheBaseline(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	step := strings.Repeat("step ", 800)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: "the request"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	a.appendMessage(llm.Message{Role: "assistant", Content: step})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail"})
	_ = a.ContextStats(context.Background())
	local := a.compactionTokenTotal()
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()
	if local >= budget {
		t.Fatalf("test setup: local total %d already at the budget %d", local, budget)
	}
	a.recordTurnUsage(&llm.Usage{PromptTokens: budget + 500})
	if total := a.compactionTokenTotal(); total < budget || total >= hard {
		t.Fatalf("test setup: total %d not in [budget %d, hard limit %d)", total, budget, hard)
	}

	if !a.compactIfDue(context.Background(), nil) {
		t.Fatal("the unchanged first keep stopped the sequence")
	}
	if hasContent(a.Messages, step) {
		t.Fatal("the recent work is still verbatim")
	}
	if !hasContent(a.Messages, "the request") {
		t.Fatal("the request was summarized")
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestEmergencyNotSmallerTriesSmallerKeep pins a progress region whose
// summary comes back no smaller. The next keep includes the tail and
// summarizes it. The refusal does not back off while that keep remains.
func TestEmergencyNotSmallerTriesSmallerKeep(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	provider.hugeFirst = true
	a.SetDeepDive(true)
	request := "the request"
	tail := strings.Repeat("y", 90000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("w", 4000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: tail})
	_ = a.ContextStats(context.Background())
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if !hasContent(a.Messages, request) {
		t.Fatal("the request was summarized")
	}
	if hasContent(a.Messages, tail) {
		t.Fatal("the tail is still verbatim")
	}
	if n := atomic.LoadInt64(&provider.calls); n < 2 {
		t.Fatalf("summarizer calls = %d, want the refused region and the next keep", n)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("the refusal was recorded: backoff %v guard %d", backoff, guard)
	}
}

// TestCompactToFitCancelSkipsFailure pins a cancel during the shrinking
// tail, and one during the last-keep single summary. Neither is stored
// as a compact failure or sets the emergency guard.
func TestCompactToFitCancelSkipsFailure(t *testing.T) {
	t.Run("during the keep", func(t *testing.T) {
		a, _, provider := newEmergencyTestAgent(t, false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		provider.cancel = cancel
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 30000)})
		a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 30000)})
		_ = a.ContextStats(context.Background())

		a.compactToFit(ctx, nil, cachedCounts(t, a), 0, true)
		if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
			t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
		}
		if !hasContent(a.Messages, strings.Repeat("x", 30000)) {
			t.Fatal("the history changed after the cancel")
		}
	})
	t.Run("during the single summary", func(t *testing.T) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		provider.cancel = cancel
		provider.cancelAfter = 2
		request := strings.Repeat("x", 120000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: request})
		a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
		_ = a.ContextStats(context.Background())
		if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
			t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
		}

		a.compactToFit(ctx, nil, cachedCounts(t, a), 0, true)
		if !hasContent(a.Messages, request) {
			t.Fatal("the single summary completed after the cancel")
		}
		if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
			t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
		}
	})
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
	idx, c := largestCondensableMessage(msgs, counts)
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
	idx, _ := largestCondensableMessage(msgs, counts)
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
	largest, _ := largestCondensableMessage(msgs, counts)
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

// TestEmergencyOverWindowLeavesRequestForCondensation pins the last keep.
// The view is over the window and condensing the tool result would fit, so
// the emergency tier does not summarize the request.
func TestEmergencyOverWindowLeavesRequestForCondensation(t *testing.T) {
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

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 0, true)
	if !hasContent(a.Messages, request) {
		t.Fatal("the emergency tier summarized the request")
	}
	if !hasContent(a.Messages, strings.Repeat("x", 64000)) {
		t.Fatal("the tool result was summarized instead of left for condensation")
	}
	if _, guard := compactState(a); guard != 0 {
		t.Fatal("leaving the tool for preflight armed the emergency guard")
	}
	if backoff, _ := compactState(a); !backoff.IsZero() {
		t.Fatal("leaving the tool for condensation recorded a backoff")
	}

	before := atomic.LoadInt64(&provider.calls)
	out, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView())
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("preflight summarized the request")
	}
	condensed := false
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			condensed = true
		}
		if strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) {
			t.Fatal("preflight summarized the tool instead of condensing it")
		}
	}
	if !condensed {
		t.Fatal("preflight did not condense the tool result")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window", est)
	}
	if atomic.LoadInt64(&provider.calls) <= before {
		t.Fatal("preflight did not condense")
	}
}

// TestEmergencySmallerSummaryDoesNotEatTheTool pins a summary that is
// smaller than the tool result and would be published. The view would
// still be over the window, so the emergency tier must not publish it.
// The tool stays verbatim for condensation.
func TestEmergencySmallerSummaryDoesNotEatTheTool(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	toolBody := strings.Repeat("x", 64000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if !a.leaveForCondensation() {
		t.Fatal("test setup: condensation would not keep the request")
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, true)
	if atomic.LoadInt64(&provider.calls) != 0 {
		t.Fatal("the emergency tier summarized before deciding to condense")
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, toolBody) {
		t.Fatal("the summary consumed the tool or the request")
	}
	if _, guard := compactState(a); guard != 0 {
		t.Fatal("leaving the tool for preflight armed the emergency guard")
	}
}

// TestEmergencyHeadCondensesInsteadOfRequest pins a condensable first
// user message. Compaction never summarizes the head, so the last keep
// must not replace the latest request either.
func TestEmergencyHeadCondensesInsteadOfRequest(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	head := strings.Repeat("x", 64000)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if !a.leaveForCondensation() {
		t.Fatal("test setup: the head would not be condensed")
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 0, true)
	if atomic.LoadInt64(&provider.calls) != 0 {
		t.Fatal("the last keep summarized the request")
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, head) {
		t.Fatal("the request or the head was replaced")
	}
	if backoff, guard := compactState(a); guard != 0 || !backoff.IsZero() {
		t.Fatal("leaving the head armed the emergency guard")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView())
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) || hasContent(a.Messages, head) {
		t.Fatal("preflight did not condense the head and keep the request")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window", est)
	}
}

// TestEmergencyOtherSummaryDoesNotClearTheWindow pins an assistant
// message between a condensable head and the request. Summarizing that
// assistant can drop the view under the window while it stays over the
// hard limit, and the last keep would then summarize the request. The
// emergency tier publishes neither summary.
func TestEmergencyOtherSummaryDoesNotClearTheWindow(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	head := strings.Repeat("x", 64000)
	middle := strings.Repeat("y", 8000)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if !a.leaveForCondensation() {
		t.Fatal("test setup: the head would not be condensed")
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, true)
	if atomic.LoadInt64(&provider.calls) != 0 {
		t.Fatal("the emergency tier summarized before condensation")
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, head) || !hasContent(a.Messages, middle) {
		t.Fatal("a summary replaced the request, the head, or the assistant message")
	}
	if backoff, guard := compactState(a); guard != 0 || !backoff.IsZero() {
		t.Fatal("leaving the head armed the emergency guard")
	}

	if _, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView()); err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, middle) || hasContent(a.Messages, head) {
		t.Fatal("preflight did not condense the head and keep the rest")
	}
}

// TestToolRoundPreflightCondensesWithoutSummarizing pins a tool round
// with smart compact off. Preflight is the first summarizer, and it must
// condense the head instead of summarizing the assistant message.
func TestToolRoundPreflightCondensesWithoutSummarizing(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	head := strings.Repeat("x", 64000)
	middle := strings.Repeat("y", 8000)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "tool-result"})
	_ = a.ContextStats(context.Background())
	if a.smartCompactEnabled() {
		t.Fatal("test setup: smart compact is on")
	}
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if !a.leaveForCondensation() {
		t.Fatal("test setup: the head would not be condensed")
	}

	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&provider.calls) == 0 {
		t.Fatal("preflight did not condense")
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, middle) || hasContent(a.Messages, head) {
		t.Fatal("preflight summarized instead of condensing the head")
	}
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) || strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			t.Fatal("preflight published a compaction summary")
		}
	}
}

// TestColdCacheNormalTierLeavesTheHead pins a turn whose token counts
// were dropped. The emergency tier stays idle, and the normal tier must
// not publish a summary while the head can be condensed.
func TestColdCacheNormalTierLeavesTheHead(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	head := strings.Repeat("x", 64000)
	middle := strings.Repeat("y", 8000)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if !a.leaveForCondensation() {
		t.Fatal("test setup: the head would not be condensed")
	}
	a.statsMu.Lock()
	a.tokenCounts = nil
	a.statsMu.Unlock()
	if a.compactionTokenTotal() != -1 {
		t.Fatal("test setup: the count cache is still complete")
	}
	if a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("test setup: the emergency tier is due without counts")
	}
	if !a.shouldCompactUsingCounts() {
		t.Fatal("test setup: the normal tier would not run")
	}

	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, middle) || hasContent(a.Messages, head) {
		t.Fatal("the normal tier summarized instead of leaving the head")
	}
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) || strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			t.Fatal("the normal tier published a compaction summary")
		}
	}
	if atomic.LoadInt64(&provider.calls) == 0 {
		t.Fatal("preflight did not condense the head")
	}
}

// TestEmergencyLaterPinLeavesEarlierTool pins a tool result that a later
// pin holds in the tail. The pin does not cover that earlier result, so
// the last keep must leave it, and the request, for condensation.
func TestEmergencyLaterPinLeavesEarlierTool(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	toolBody := strings.Repeat("x", 64000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_2", Content: "pinned-tail"})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	pm := NewPinManager()
	pm.pinned[len(a.Messages)-1] = struct{}{}
	a.PinManager = pm
	pins := pm.PinnedSet()
	if !a.leaveForCondensation() {
		t.Fatal("test setup: the earlier tool would not be condensed")
	}

	a.compactToFit(context.Background(), pins, cachedCounts(t, a), 0, true)
	if atomic.LoadInt64(&provider.calls) != 0 {
		t.Fatal("the last keep summarized the request")
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, toolBody) || !hasContent(a.Messages, "pinned-tail") {
		t.Fatal("the request, the tool, or the pin was replaced")
	}
	if backoff, guard := compactState(a); guard != 0 || !backoff.IsZero() {
		t.Fatal("leaving the tool armed the emergency guard")
	}

	if _, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView()); err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) || !hasContent(a.Messages, "pinned-tail") {
		t.Fatal("preflight summarized the request or the pinned tool")
	}
	condensed := false
	for _, m := range a.Messages {
		if m.Role == "tool" && m.ToolCallID == "call_1" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			condensed = true
		}
	}
	if !condensed {
		t.Fatal("preflight did not condense the earlier tool result")
	}
}

// TestEmergencyPinIsNotCondensed pins a tool result. The request's own
// replacement fits, so last resort does not reach into the pin. The
// request is summarized and the tool stays.
func TestEmergencyPinIsNotCondensed(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 80000)
	toolBody := strings.Repeat("x", 100000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	pm := NewPinManager()
	pm.pinned[len(a.Messages)-1] = struct{}{}
	a.PinManager = pm
	if a.leaveForCondensation() {
		t.Fatal("test setup: a pin is still a condensation candidate")
	}

	a.compactToFit(context.Background(), pm.PinnedSet(), cachedCounts(t, a), 2, true)
	if hasContent(a.Messages, request) {
		t.Fatal("the emergency tier left the request verbatim when the tool is pinned")
	}
	if !hasContent(a.Messages, toolBody) {
		t.Fatal("the pinned tool was summarized")
	}
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("the emergency tier condensed a message")
		}
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView())
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, toolBody) {
		t.Fatal("preflight condensed the pinned tool")
	}
	for _, m := range out {
		if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("preflight condensed a pinned message")
		}
	}
}

// TestPinnedSuffixOverTheWindowIsCondensed pins the stretch a pin keeps.
// Compaction will not summarize it. When nothing outside it fits, last
// resort condenses the largest message inside it, in deep dive and after
// a provider refusal. Context mode off already condenses that message.
// An unpinned message that fits is still preferred, which
// TestEmergencyPinIsNotCondensed pins.
func TestPinnedSuffixOverTheWindowIsCondensed(t *testing.T) {
	t.Run("deep dive condenses the pinned request", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		request := "keep-this-request\n" + strings.Repeat("x", 200000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: request})
		_ = a.ContextStats(context.Background())
		pm := NewPinManager()
		pm.pinned[1] = struct{}{}
		a.PinManager = pm
		if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate under the window")
		}

		out, err := a.prepareMessages(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if hasContent(a.Messages, request) {
			t.Fatal("the pinned request was left verbatim")
		}
		if a.Messages[0].Content != "hello" {
			t.Fatal("the head changed")
		}
		if !pinnedCondensed(pm, a.Messages) {
			t.Fatal("the condensed request is not the pinned message")
		}
		if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
			t.Fatalf("estimate %d still over the window", est)
		}
		assertCompactionRecovered(t, a)
	})

	t.Run("a pin in front of an oversized tool", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		toolBody := strings.Repeat("x", 200000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: "the request"})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		pm := NewPinManager()
		pm.pinned[1] = struct{}{}
		a.PinManager = pm
		if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate under the window")
		}

		out, err := a.prepareMessages(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !hasContent(a.Messages, "the request") || !hasContent(a.Messages, "hello") {
			t.Fatal("the head or the pinned request was replaced")
		}
		if hasContent(a.Messages, toolBody) {
			t.Fatal("the oversized tool behind the pin was left verbatim")
		}
		if !toolWasCondensed(a.Messages) {
			t.Fatal("the tool behind the pin was summarized instead of condensed")
		}
		if !pm.IsPinned(1) {
			t.Fatal("condensing the tool dropped the pin on the request")
		}
		if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
			t.Fatalf("estimate %d still over the window", est)
		}
		assertCompactionRecovered(t, a)
	})

	// Last resort runs only while the estimate is still over the window.
	// Pre-flight is the usual caller. This is the overflow caller of the
	// same choice, after a forced summary that changed nothing.
	t.Run("overflow condenses the pinned tool", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		toolBody := strings.Repeat("x", 200000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: "the request"})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		pm := NewPinManager()
		pm.pinned[1] = struct{}{}
		a.PinManager = pm
		if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate under the window")
		}

		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if err != nil || !retry {
			t.Fatalf("retry = %v, err = %v", retry, err)
		}
		if !hasContent(a.Messages, "the request") || !hasContent(a.Messages, "hello") || hasContent(a.Messages, toolBody) {
			t.Fatal("overflow left the pinned tool verbatim or replaced the request")
		}
		if !toolWasCondensed(a.Messages) {
			t.Fatal("overflow summarized the tool instead of condensing it")
		}
		if !pm.IsPinned(1) {
			t.Fatal("condensing the tool dropped the pin on the request")
		}
		if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
			t.Fatalf("estimate %d still over the window", est)
		}
		assertCompactionRecovered(t, a)
	})

	t.Run("context mode off condenses a pinned request", func(t *testing.T) {
		a, ctxMgr, _ := newEmergencyTestAgent(t, false)
		request := "keep-this-request\n" + strings.Repeat("x", 200000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "user", Content: request})
		_ = a.ContextStats(context.Background())
		pm := NewPinManager()
		pm.pinned[1] = struct{}{}
		a.PinManager = pm
		if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate under the window")
		}

		out, err := a.prepareMessages(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if hasContent(a.Messages, request) {
			t.Fatal("context mode off left the pinned request verbatim")
		}
		if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
			t.Fatalf("estimate %d still over the window", est)
		}
		assertCompactionRecovered(t, a)
	})
}

func assertCompactionRecovered(t *testing.T, a *Agent) {
	t.Helper()
	a.statsMu.RLock()
	emergency := a.compactionGuards.emergency
	lastResort := a.compactionGuards.lastResort
	until := a.compactBackoffUntil
	a.statsMu.RUnlock()
	if emergency != 0 || lastResort != 0 || !until.IsZero() {
		t.Fatalf("after condensation emergency=%d lastResort=%d backoff=%s", emergency, lastResort, until)
	}
}

func pinnedCondensed(pm *PinManager, msgs []llm.Message) bool {
	for i, m := range msgs {
		if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) && pm.IsPinned(i) {
			return true
		}
	}
	return false
}

func toolWasCondensed(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			return true
		}
	}
	return false
}

// TestSmartCompactThenPreflightCondensesTheTool pins the path after a tool
// batch. Smart compact leaves the tool, the batch appends messages, and the
// next model-call setup condenses the tool. The request stays.
func TestSmartCompactThenPreflightCondensesTheTool(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	enableSmartCompact(a)
	request := "keep-this-request\n" + strings.Repeat("x", 104000)
	toolBody := strings.Repeat("x", 64000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	res := &llm.StreamResult{ToolCalls: []llm.ToolCall{{
		ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
	}}}
	if action := a.smartCompactBeforeTools(context.Background(), nil, res); action != smartCompactExecute {
		t.Fatalf("action = %d, want execute", action)
	}
	if !hasContent(a.Messages, toolBody) {
		t.Fatal("smart compact summarized the tool")
	}
	a.appendMessage(llm.Message{Role: "assistant", Content: "running", ToolCalls: res.ToolCalls})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_2", Content: "new result"})

	out, err := a.prepareMessages(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the next setup summarized the request")
	}
	condensed := false
	for _, m := range a.Messages {
		if m.Role == "tool" && m.ToolCallID == "call_1" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			condensed = true
		}
	}
	if !condensed {
		t.Fatal("the next setup did not condense the tool result")
	}
	if est := a.outgoingViewEstimate(out); est >= ctxMgr.ContextLimit() {
		t.Fatalf("estimate %d still over the window", est)
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

// TestDefaultModeUnderWindowCondensesTheTool pins context mode off.
// The provider refuses while the estimate is under the window. There is
// no middle to summarize. The trailing tool is the message compaction
// would leave verbatim, and it is condensed. The retry is that
// condensation. The emergency guard and the wait stay clear.
func TestDefaultModeUnderWindowCondensesTheTool(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	toolBody := strings.Repeat("x", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate is over the window")
	}
	if !condensableTailFits(a, ctxMgr.ContextLimit()) {
		t.Fatal("test setup: the tool result is not a condensation that fits")
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil || !retry {
		t.Fatalf("retry = %v, err = %v", retry, err)
	}
	if hasContent(a.Messages, toolBody) || !hasContent(a.Messages, "hello") {
		t.Fatal("default mode left the tool verbatim or dropped the head")
	}
	if !toolWasCondensed(a.Messages) {
		t.Fatal("default mode did not condense the tool")
	}
	assertCompactionRecovered(t, a)
}

// TestPreservedMessageIsMeasuredAgainstTheWholeMiddle pins a trailing
// tool that is larger than each middle message and smaller than those
// messages together. The summary replaces that whole middle, so the tool
// stays and the middle is summarized.
func TestPreservedMessageIsMeasuredAgainstTheWholeMiddle(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	middles := []string{
		"one " + strings.Repeat("w", 3000),
		"two " + strings.Repeat("w", 3000),
		"three " + strings.Repeat("w", 3000),
	}
	toolBody := strings.Repeat("w", 5000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	for _, middle := range middles {
		a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est >= ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d is over the window", est)
	}
	msgs, counts := a.messageCounts()
	var toolTok, oneTok, sum int
	for i, m := range msgs {
		if m.Role == "tool" {
			toolTok = counts[i]
		}
		if strings.HasPrefix(m.Content, "one ") || strings.HasPrefix(m.Content, "two ") || strings.HasPrefix(m.Content, "three ") {
			if counts[i] > oneTok {
				oneTok = counts[i]
			}
			sum += counts[i]
		}
	}
	if toolTok <= oneTok || toolTok >= sum {
		t.Fatalf("test setup: tool %d, largest middle %d, sum %d", toolTok, oneTok, sum)
	}
	if got := a.preservedLargerThanMiddle(); got != 0 {
		t.Fatalf("preserved larger = %d, want 0", got)
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil || !retry {
		t.Fatalf("retry = %v, err = %v", retry, err)
	}
	if !hasContent(a.Messages, toolBody) {
		t.Fatal("the smaller tool was condensed")
	}
	kept := 0
	for _, middle := range middles {
		if hasContent(a.Messages, middle) {
			kept++
		}
	}
	if kept == len(middles) {
		t.Fatal("the larger middle was not summarized")
	}
}

// TestShrunkHistoryLowersTheSingleSummaryGuard pins a failed single
// summary whose history is then shortened. The failure stays, at the new
// count. One new message re-arms it. The old count does not.
func TestShrunkHistoryLowersTheSingleSummaryGuard(t *testing.T) {
	a, _, _ := newEmergencyTestAgent(t, false)
	for i := 0; i < 6; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: "m" + string(rune('0'+i))})
	}
	a.noteSingleSummaryFailure()
	if !a.singleSummaryFailed() {
		t.Fatal("the failure was not recorded")
	}
	a.publishCompaction(append([]llm.Message(nil), a.Messages[:3]...), nil)
	if !a.singleSummaryFailed() {
		t.Fatal("the shrink cleared the failure")
	}
	a.appendMessage(llm.Message{Role: "user", Content: "again"})
	if a.singleSummaryFailed() {
		t.Fatal("the old count still blocks the summary")
	}
}

// TestEmergencyHonorsAFailedSingleSummary pins the emergency tier. A
// single summary that already failed is not run again, so the request
// stays and the wait is not doubled.
func TestEmergencyHonorsAFailedSingleSummary(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 120000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}
	a.noteSingleSummaryFailure()
	a.statsMu.Lock()
	a.compactBackoffDelay = 30 * time.Second
	a.compactBackoffUntil = time.Now().Add(time.Hour)
	a.statsMu.Unlock()

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if !hasContent(a.Messages, big) {
		t.Fatal("the emergency tier repeated the failed single summary")
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want 30s", a.compactBackoffDelay)
	}
}

// TestDefaultModeNotSmallerArmsBackoff pins a summary that comes back no
// smaller with context mode off. It backs off and arms the emergency
// guard. The history stays, including a tool result deep dive would condense.
func TestDefaultModeNotSmallerArmsBackoff(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	provider.hugeFirst = true
	middle := strings.Repeat("w", 4000)
	toolBody := strings.Repeat("x", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate is over the window")
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if retry {
		t.Fatal("retried a summary that was not smaller")
	}
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("err = %v, want the original refusal", err)
	}
	if !hasContent(a.Messages, middle) || !hasContent(a.Messages, toolBody) {
		t.Fatal("the history changed")
	}
	backoff, guard := compactState(a)
	if guard == 0 || backoff.IsZero() {
		t.Fatalf("guard = %d, backoff %v; want both set", guard, backoff)
	}
}

// TestMissedCondensationDoesNotRetryTheMiddle pins a provider refusal
// under the window whose larger preserved message comes back no smaller.
// The smaller middle is not summarized, and the only retry is not spent.
// The miss is not recorded, so a later summary can be smaller.
func TestMissedCondensationDoesNotRetryTheMiddle(t *testing.T) {
	assertMissed := func(t *testing.T, a *Agent, kept ...string) {
		t.Helper()
		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if retry {
			t.Fatal("retried after a condensation that was not smaller")
		}
		if !errors.Is(err, llm.ErrContextWindowExceeded) {
			t.Fatalf("err = %v, want the original refusal", err)
		}
		for _, body := range kept {
			if !hasContent(a.Messages, body) {
				t.Fatal("the history changed")
			}
		}
		for _, m := range a.Messages {
			if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) || contextmgr.IsCompactionSummary(m.Content) {
				t.Fatal("a summary was published")
			}
		}
		if got := a.overflowRetries.Load(); got != 0 {
			t.Fatalf("overflow retries = %d, want 0", got)
		}
		if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
			t.Fatalf("the miss was recorded: backoff %v guard %d", backoff, guard)
		}
	}
	t.Run("tool larger than the middle", func(t *testing.T) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		provider.hugeFirst = true
		middle := strings.Repeat("w", 4000)
		toolBody := strings.Repeat("w", 8000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "assistant", Content: middle})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate is over the window")
		}
		msgs, counts := a.messageCounts()
		var toolTok, midTok int
		for i, m := range msgs {
			if m.Role == "tool" {
				toolTok = counts[i]
			}
			if m.Content == middle {
				midTok = counts[i]
			}
		}
		if toolTok <= midTok {
			t.Fatalf("test setup: tool %d is not larger than the middle %d", toolTok, midTok)
		}
		assertMissed(t, a, middle, toolBody)
	})
	t.Run("head larger than the middle", func(t *testing.T) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		provider.hugeFirst = true
		head := strings.Repeat("w", 8000)
		middle := strings.Repeat("w", 4000)
		a.appendMessage(llm.Message{Role: "user", Content: head})
		a.appendMessage(llm.Message{Role: "assistant", Content: middle})
		a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
		_ = a.ContextStats(context.Background())
		if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate is over the window")
		}
		assertMissed(t, a, head, middle)
	})
	t.Run("deep dive", func(t *testing.T) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		provider.hugeFirst = true
		middle := strings.Repeat("w", 4000)
		toolBody := strings.Repeat("w", 8000)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "assistant", Content: middle})
		a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
		_ = a.ContextStats(context.Background())
		if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate is over the window")
		}
		assertMissed(t, a, "hello", middle, toolBody)
	})
}

// TestDefaultModeKeepsLargestCondensation pins context mode off. A smaller
// tool result would fit, and the largest message's truncation would not.
// Default mode does not switch to that tool. Deep dive does, in
// TestPreflightSmallerMessageKeepsRequest.
func TestDefaultModeKeepsLargestCondensation(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	head := strings.Repeat("x", 104000)
	toolBody := strings.Repeat("x", 64000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
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
	largest, largestTok := largestCondensableMessage(msgs, counts)
	if largest < 0 || msgs[largest].Content != head {
		t.Fatal("test setup: the head is not the largest condensable message")
	}
	if total-largestTok+condensedReplacementTokens(a, msgs[largest]) < limit {
		t.Fatal("test setup: the largest replacement fits, so default mode would condense it")
	}
	var toolTok int
	for i, m := range msgs {
		if m.Role == "tool" {
			toolTok = counts[i]
		}
	}
	if total-toolTok+lastResortCondenseAllowance >= limit {
		t.Fatal("test setup: condensing the tool does not fit")
	}

	out, err := a.preflightForcedCompact(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, head) {
		t.Fatal("default mode condensed the head")
	}
	for _, m := range a.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("default mode condensed the smaller tool result")
		}
	}
	if est := a.outgoingViewEstimate(out); est < limit {
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

// TestStillOverBackoffFollowsTheDeadline pins a give-up that leaves the
// view over the window. A wait that has already ended is recorded again
// with the doubled delay, so the next tool batch does not compact in the
// normal tier. A wait that is still running is not extended.
func TestStillOverBackoffFollowsTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		until time.Time
		renew bool
	}{
		{name: "elapsed", until: time.Now().Add(-time.Minute), renew: true},
		{name: "running", until: time.Now().Add(2 * time.Hour), renew: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ctxMgr, provider := newEmergencyTestAgent(t, false)
			a.SetDeepDive(true)
			enableSmartCompact(a)
			provider.hugeFirst = true
			request := "keep-this-request\n" + strings.Repeat("x", 104000)
			toolBody := strings.Repeat("x", 64000)
			a.appendMessage(llm.Message{Role: "user", Content: "hello"})
			a.appendMessage(llm.Message{Role: "user", Content: request})
			a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
			a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
			_ = a.ContextStats(context.Background())
			if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
				t.Fatal("test setup: estimate under the window")
			}
			if !a.leaveForCondensation() {
				t.Fatal("test setup: condensation would not keep the request")
			}
			a.statsMu.Lock()
			a.compactBackoffDelay = 30 * time.Second
			a.compactBackoffUntil = tc.until
			a.statsMu.Unlock()

			if _, err := a.prepareMessages(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if !hasContent(a.Messages, request) || !hasContent(a.Messages, toolBody) {
				t.Fatal("preflight replaced the request or the tool")
			}
			got, _ := compactState(a)
			if tc.renew {
				if a.compactAttemptDue() || !got.After(time.Now()) {
					t.Fatal("an elapsed backoff was not recorded again")
				}
				if a.compactBackoffDelay != 60*time.Second {
					t.Fatalf("backoff delay = %s, want the doubled 60s", a.compactBackoffDelay)
				}
			} else if !got.Equal(tc.until) {
				t.Fatalf("running backoff moved from %v to %v", tc.until, got)
			} else if a.compactBackoffDelay != 30*time.Second {
				t.Fatalf("running backoff delay = %s, want 30s", a.compactBackoffDelay)
			}

			res := &llm.StreamResult{ToolCalls: []llm.ToolCall{{
				ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
			}}}
			a.smartCompactBeforeTools(context.Background(), nil, res)
			if !hasContent(a.Messages, request) || !hasContent(a.Messages, toolBody) {
				t.Fatal("the next tool batch summarized the message condensation was keeping")
			}
		})
	}
}

// TestFailedSingleSummaryIsNotRepeated pins a single summary that comes
// back too big. Preflight and the provider refusal must not run it again,
// and the backoff is recorded once.
func TestFailedSingleSummaryIsNotRepeated(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.hugeFirst = true
	// Both messages stay over the window after truncation, so last resort
	// publishes nothing. Together they are still smaller than the first
	// huge summary, so that summary is refused.
	head := strings.Repeat("x", 130000)
	request := "keep-this-request\n" + strings.Repeat("x", 130000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if a.leaveForCondensation() {
		t.Fatal("test setup: condensation would fit, so the single summary is not the path")
	}

	if _, err := a.preflightForcedCompact(context.Background(), nil, a.rebuildView()); err != nil {
		t.Fatal(err)
	}
	if calls := atomic.LoadInt64(&provider.calls); calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", calls)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("the repeated summary replaced the request")
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want 30s", a.compactBackoffDelay)
	}

	_, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if err != nil && !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatal(err)
	}
	if calls := atomic.LoadInt64(&provider.calls); calls != 1 {
		t.Fatalf("summarizer calls after the refusal = %d, want 1", calls)
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("overflow repeated the failed summary")
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay after the refusal = %s, want 30s", a.compactBackoffDelay)
	}
}

// TestExhaustedGiveUpLeavesARunningBackoff pins an emergency give-up
// whose keeps publish nothing while a wait is already running and the
// view is over the window. The wait stays put, and the guard is set.
func TestExhaustedGiveUpLeavesARunningBackoff(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 200000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail"})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("setup: view is under the window")
	}
	until := time.Now().Add(2 * time.Hour)
	a.statsMu.Lock()
	a.compactBackoffDelay = 30 * time.Second
	a.compactBackoffUntil = until
	a.statsMu.Unlock()

	// The pin sits on the message after the head, so every keep has an
	// empty middle and nothing is published.
	a.compactToFit(context.Background(), map[int]struct{}{1: {}}, cachedCounts(t, a), 2, true)

	if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", calls)
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want 30s", a.compactBackoffDelay)
	}
	got, guard := compactState(a)
	if !got.Equal(until) {
		t.Fatalf("running backoff moved from %v to %v", until, got)
	}
	if guard != len(a.Messages) {
		t.Fatalf("guard = %d, want %d", guard, len(a.Messages))
	}
}

// TestBudgetMissAtHardLimitSetsTheGuard pins an emergency give-up in
// context mode off. The view is under the window and still at or above
// the hard limit, which is what makes the emergency tier due. The wait
// is recorded and the guard is set, so the next check does not run it
// again or double the wait.
func TestBudgetMissAtHardLimitSetsTheGuard(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 112000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail"})
	_ = a.ContextStats(context.Background())
	view := a.outgoingViewEstimate(a.rebuildView())
	limit := ctxMgr.ContextLimit()
	if view >= limit || view < ctxMgr.HardLimit() {
		t.Fatalf("setup: view %d not in [hard limit %d, window %d)", view, ctxMgr.HardLimit(), limit)
	}

	a.compactToFit(context.Background(), map[int]struct{}{1: {}}, cachedCounts(t, a), 2, true)

	if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", calls)
	}
	if _, guard := compactState(a); guard != len(a.Messages) {
		t.Fatalf("guard = %d, want %d", guard, len(a.Messages))
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want 30s", a.compactBackoffDelay)
	}
	if backoff, _ := compactState(a); backoff.IsZero() || !backoff.After(time.Now()) {
		t.Fatal("budget miss did not start a wait")
	}
	if a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("the emergency tier is due again at the same count")
	}
	if a.compactIfDue(context.Background(), nil) {
		t.Fatal("the next check ran compaction")
	}
	if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
		t.Fatalf("summarizer calls after the next check = %d, want 0", calls)
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want the same 30s", a.compactBackoffDelay)
	}
}

// TestDeepDiveNotSmallerTriesSmallerKeep pins a summary that is not
// smaller while a smaller keep remains. Deep dive runs that keep. The
// wait stays unset when the later keep fits.
func TestDeepDiveNotSmallerTriesSmallerKeep(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.hugeFirst = true
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("x", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("x", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail one"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail two"})
	_ = a.ContextStats(context.Background())

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, false)
	if calls := atomic.LoadInt64(&provider.calls); calls < 2 {
		t.Fatalf("summarizer calls = %d, want at least 2", calls)
	}
	if strings.Contains(summaryBlob(a.Messages), strings.Repeat("z", 8000)) {
		t.Fatal("the not-smaller summary was published")
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("backoff %v guard %d, want both clear", backoff, guard)
	}
	found := false
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the later keep did not publish a summary")
	}
}

// TestDeepDiveExhaustedGiveUpLeavesARunningBackoff pins a deep-dive
// give-up while a wait is running and the view is under the window. The
// deadline stays put and the emergency guard is set.
func TestDeepDiveExhaustedGiveUpLeavesARunningBackoff(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	// One user message, so the last keep does not single-summarize a
	// later request. The pin empties the middle.
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("x", 112000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail"})
	_ = a.ContextStats(context.Background())
	if view, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); view >= limit {
		t.Fatalf("setup: view %d is over the window %d", view, limit)
	}
	until := time.Now().Add(2 * time.Hour)
	a.statsMu.Lock()
	a.compactBackoffDelay = 30 * time.Second
	a.compactBackoffUntil = until
	a.statsMu.Unlock()

	a.compactToFit(context.Background(), map[int]struct{}{1: {}}, cachedCounts(t, a), 2, true)

	if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", calls)
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want 30s", a.compactBackoffDelay)
	}
	got, guard := compactState(a)
	if !got.Equal(until) {
		t.Fatalf("running backoff moved from %v to %v", until, got)
	}
	if guard != len(a.Messages) {
		t.Fatalf("guard = %d, want %d", guard, len(a.Messages))
	}
}

// TestExhaustedGiveUpOverWindowStartsTheWait pins a give-up in context
// mode off with no earlier wait and the view over the window. The wait
// starts at 30 seconds and the guard is set.
func TestExhaustedGiveUpOverWindowStartsTheWait(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 200000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail"})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("setup: view is under the window")
	}

	a.compactToFit(context.Background(), map[int]struct{}{1: {}}, cachedCounts(t, a), 2, true)

	if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", calls)
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want 30s", a.compactBackoffDelay)
	}
	backoff, guard := compactState(a)
	if backoff.IsZero() || !backoff.After(time.Now()) {
		t.Fatal("the give-up did not start a wait")
	}
	if guard != len(a.Messages) {
		t.Fatalf("guard = %d, want %d", guard, len(a.Messages))
	}
}

// TestDefaultColdCacheCompactsUnderTheWindow pins a missing token cache
// in context mode off, with the view under the window. The normal tier
// compacts. Preflight does not condense.
func TestDefaultColdCacheCompactsUnderTheWindow(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	middle := strings.Repeat("x", 90000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "user", Content: "latest"})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate is over the window")
	}
	a.statsMu.Lock()
	a.tokenCounts = nil
	a.statsMu.Unlock()
	if a.compactionTokenTotal() != -1 {
		t.Fatal("test setup: the count cache is still complete")
	}
	if a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("test setup: the emergency tier is due without counts")
	}
	if !a.shouldCompactUsingCounts() {
		t.Fatal("test setup: the normal tier would not run")
	}

	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, middle) {
		t.Fatal("the normal tier left the middle verbatim")
	}
	found := false
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			found = true
		}
		if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			t.Fatal("preflight condensed a message under the window")
		}
	}
	if !found {
		t.Fatal("the normal tier did not publish a summary")
	}
	if atomic.LoadInt64(&provider.calls) == 0 {
		t.Fatal("the normal tier made no summarizer call")
	}
}

// TestForcedSummaryNoShrinkRecordsOneWait pins a forced summary that
// changes nothing while the view is over the window. The history stays,
// the emergency guard is set, and the wait is recorded once.
func TestForcedSummaryNoShrinkRecordsOneWait(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	head := strings.Repeat("x", 130000)
	toolBody := strings.Repeat("x", 130000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	pm := NewPinManager()
	pm.pinned[1] = struct{}{}
	a.PinManager = pm

	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !hasContent(a.Messages, head) || !hasContent(a.Messages, toolBody) {
		t.Fatal("a no-shrink pass replaced the history")
	}
	if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", calls)
	}
	if a.compactBackoffDelay != 30*time.Second {
		t.Fatalf("backoff delay = %s, want one 30s wait", a.compactBackoffDelay)
	}
	if _, guard := compactState(a); guard != len(a.Messages) {
		t.Fatalf("guard = %d, want %d", guard, len(a.Messages))
	}
}

// TestThresholdZeroSkipsTiersAndKeepsPreflight pins compact_threshold 0.
// Neither tier calls the summarizer under the window. Preflight still
// runs when the view is over the window.
func TestThresholdZeroSkipsTiersAndKeepsPreflight(t *testing.T) {
	t.Run("under the window", func(t *testing.T) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		s := ctxMgr.SettingsSnapshot()
		s.CompactThreshold = 0
		ctxMgr.UpdateSettings(s)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("x", 90000)})
		a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
		a.appendMessage(llm.Message{Role: "user", Content: "latest"})
		_ = a.ContextStats(context.Background())
		if a.outgoingViewEstimate(a.rebuildView()) >= ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate is over the window")
		}
		if a.Context.AutoCompactEnabled() {
			t.Fatal("test setup: auto-compact is on")
		}

		if _, err := a.prepareMessages(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if calls := atomic.LoadInt64(&provider.calls); calls != 0 {
			t.Fatalf("summarizer calls = %d, want 0", calls)
		}
		if _, guard := compactState(a); guard != 0 {
			t.Fatalf("guard = %d, want 0", guard)
		}
	})

	t.Run("over the window", func(t *testing.T) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		s := ctxMgr.SettingsSnapshot()
		s.CompactThreshold = 0
		ctxMgr.UpdateSettings(s)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("x", 8000)})
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 200000)})
		_ = a.ContextStats(context.Background())
		if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
			t.Fatal("test setup: estimate under the window")
		}

		if _, err := a.prepareMessages(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if calls := atomic.LoadInt64(&provider.calls); calls == 0 {
			t.Fatal("preflight made no summarizer call")
		}
	})
}

// errorModeOverWindowAgent is a deep-dive conversation whose view is over
// the window and whose head would be condensed. Error mode must not take
// that skip.
func errorModeOverWindowAgent(t *testing.T, mode string) (*Agent, *contextmgr.Manager, *countingProvider, string, string) {
	t.Helper()
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	s := ctxMgr.SettingsSnapshot()
	s.CompactLastResort = mode
	ctxMgr.UpdateSettings(s)
	a.SetDeepDive(true)
	head := "HEAD " + strings.Repeat("x", 80000)
	middle := "MIDDLE " + strings.Repeat("x", 80000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "user", Content: "keep-this-request"})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	return a, ctxMgr, provider, head, middle
}

// TestDeepDiveErrorModeSummarizesBeforeTheDiagnostic pins compact_last_resort
// error. A message whose replacement would fit does not skip the summary,
// on a user message, on a tool round, with a cold token cache, or after
// the provider refuses. Condense mode still leaves that message.
func TestDeepDiveErrorModeSummarizesBeforeTheDiagnostic(t *testing.T) {
	t.Run("condense still leaves it", func(t *testing.T) {
		a, _, _, _, _ := errorModeOverWindowAgent(t, "condense")
		if !a.leaveForCondensation() {
			t.Fatal("fixture would not have skipped the summary")
		}
	})
	for _, tc := range []struct {
		name string
		tool bool
		cold bool
	}{
		{name: "user message"},
		{name: "tool round", tool: true},
		{name: "cold cache", cold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ctxMgr, provider, head, middle := errorModeOverWindowAgent(t, "error")
			if tc.tool {
				a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
				a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "tool-result"})
				_ = a.ContextStats(context.Background())
			}
			if a.leaveForCondensation() {
				t.Fatal("error mode left the summary for a condensation that will not run")
			}
			if tc.cold {
				a.statsMu.Lock()
				a.tokenCounts = nil
				a.statsMu.Unlock()
				if a.compactionTokenTotal() != -1 {
					t.Fatal("test setup: the count cache is still complete")
				}
			}
			view, err := a.prepareMessages(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if atomic.LoadInt64(&provider.calls) == 0 {
				t.Fatal("the diagnostic was returned without a summary")
			}
			if !hasContent(a.Messages, "keep-this-request") {
				t.Fatal("the summary replaced the request")
			}
			if hasContent(a.Messages, middle) {
				t.Fatal("the middle was left for condensation")
			}
			if !hasContent(a.Messages, head) {
				t.Fatal("the summary condensed the head")
			}
			if a.outgoingViewEstimate(view) >= ctxMgr.ContextLimit() {
				t.Fatal("the summary left the view over the window")
			}
			if _, guard := compactState(a); guard != 0 {
				t.Fatal("a summary that fit set the emergency guard")
			}
		})
	}
	t.Run("overflow", func(t *testing.T) {
		a, _, provider, _, _ := errorModeOverWindowAgent(t, "error")
		retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
		if atomic.LoadInt64(&provider.calls) == 0 {
			t.Fatalf("overflow skipped the summary, retry %v err %v", retry, err)
		}
	})
}

// TestDeepDiveProgressFailureKeepsTheSessionSummary pins the agent side of
// a progress failure: the session summary is published, the work since the
// request stays, and the failure is not recorded when that result fits.
func TestDeepDiveProgressFailureKeepsTheSessionSummary(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.failAfter = 2
	old := strings.Repeat("x", 8000)
	work := strings.Repeat("y", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: old})
	a.appendMessage(llm.Message{Role: "user", Content: "the request"})
	a.appendMessage(llm.Message{Role: "assistant", Content: work})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail one"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail two"})
	_ = a.ContextStats(context.Background())

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, false)
	if hasContent(a.Messages, old) {
		t.Fatal("the session region was discarded with the progress failure")
	}
	if !hasContent(a.Messages, work) {
		t.Fatal("the progress region was dropped")
	}
	found := false
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the session summary was not published")
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("backoff %v guard %d, want both clear", backoff, guard)
	}
}

// TestSessionErrorStillPublishesTheProgressSummary pins a session summary
// that fails on the emergency tier. The progress summary is still
// published. The session region stays. The failure is not recorded when
// that result fits, so the emergency guard stays clear.
func TestSessionErrorStillPublishesTheProgressSummary(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	provider.failUnlessContains = "work done on the current request"
	old := strings.Repeat("x", 8000)
	work := strings.Repeat("y", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: old})
	a.appendMessage(llm.Message{Role: "user", Content: "the request"})
	a.appendMessage(llm.Message{Role: "assistant", Content: work})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail one"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail two"})
	_ = a.ContextStats(context.Background())

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, true)
	if !hasContent(a.Messages, old) {
		t.Fatal("the failed session summary replaced the session region")
	}
	if hasContent(a.Messages, work) {
		t.Fatal("the progress region stayed verbatim")
	}
	if calls := atomic.LoadInt64(&provider.calls); calls < 2 {
		t.Fatalf("summarizer calls = %d, want the session error and the progress summary", calls)
	}
	found := false
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) {
			found = true
		}
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			t.Fatal("the failed session summary was published")
		}
	}
	if !found {
		t.Fatal("the progress summary was not published")
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("backoff %v guard %d, want both clear", backoff, guard)
	}
}

// TestDeepDiveProgressCancelDropsBoth pins a cancel during the progress
// summary after the session summary succeeded. Neither summary is
// published. The cancel is not stored as a failure.
func TestDeepDiveProgressCancelDropsBoth(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.cancel = cancel
	provider.cancelAfter = 2
	old := strings.Repeat("x", 8000)
	work := strings.Repeat("y", 8000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: old})
	a.appendMessage(llm.Message{Role: "user", Content: "the request"})
	a.appendMessage(llm.Message{Role: "assistant", Content: work})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail one"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "tail two"})
	_ = a.ContextStats(context.Background())

	a.compactToFit(ctx, nil, cachedCounts(t, a), 2, false)
	if !hasContent(a.Messages, old) || !hasContent(a.Messages, work) {
		t.Fatal("a cancel published part of the layout")
	}
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) || strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) {
			t.Fatal("a cancel published a summary")
		}
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("cancel recorded as a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestCondensationSuccessClearsTheEmergencyGuard pins a published
// condensation. The emergency guard is cleared. The total can still be
// at the hard limit, and the view is under the window, so the emergency
// tier does not run and does not start a wait. A tool result does not
// forget the request or re-open the tier. A new user request does,
// including the same text.
func TestCondensationSuccessClearsTheEmergencyGuard(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	head := "HEAD " + strings.Repeat("x", 90000)
	request := "keep-this-request\n" + strings.Repeat("x", 110000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	if !a.leaveForCondensation() {
		t.Fatalf("test setup: the head would not be condensed (estimate %d, limit %d)", a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit())
	}

	view, err := a.prepareMessages(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, head) {
		t.Fatal("the head was not condensed")
	}
	if !hasContent(a.Messages, request) {
		t.Fatal("condensation was followed by a summary of the request")
	}
	if a.outgoingViewEstimate(view) >= ctxMgr.ContextLimit() {
		t.Fatal("condensation left the view over the window")
	}
	total := a.compactionTokenTotal()
	if total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d is under the hard limit %d, so the kept count is not what blocks the emergency tier", total, ctxMgr.HardLimit())
	}
	if _, guard := compactState(a); guard != 0 {
		t.Fatalf("emergency guard = %d, want 0", guard)
	}
	if !a.requestKeptByCondensation() {
		t.Fatal("published condensation did not remember the request")
	}
	if a.emergencyCompactDue(total) {
		t.Fatal("the emergency tier ran while the kept request is under the window")
	}
	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, true)
	if !hasContent(a.Messages, request) {
		t.Fatal("the emergency tier summarized the request condensation kept")
	}
	if backoff, _ := compactState(a); !backoff.IsZero() || a.compactBackoffDelay != 0 {
		t.Fatalf("backoff %v delay %s, want both clear", backoff, a.compactBackoffDelay)
	}

	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "tool output"})
	if !a.requestKeptByCondensation() {
		t.Fatal("a tool result forgot the request")
	}
	if a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("a tool result re-opened the emergency tier")
	}
	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, true)
	if backoff, _ := compactState(a); !backoff.IsZero() {
		t.Fatal("the tool result started a compaction wait")
	}
	a.appendMessage(llm.Message{Role: "user", Content: request})
	if a.requestKeptByCondensation() {
		t.Fatal("the same request text did not re-arm condensation's mark")
	}
}

// TestKeptRequestIsSummarizedOnceTheViewIsOver pins a condensation that
// remembered the request. Later work that is not a new user message puts
// the view over the window, and no one message's replacement fits. The
// request is summarized. Under the window the same mark still keeps it,
// which TestCondensationSuccessClearsTheEmergencyGuard pins.
func TestKeptRequestIsSummarizedOnceTheViewIsOver(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	request := "keep-this-request\n" + strings.Repeat("x", 160000)
	toolBody := strings.Repeat("y", 70000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file"}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: toolBody})
	_ = a.ContextStats(context.Background())
	a.noteCondensationPublished()
	if !a.requestKeptByCondensation() {
		t.Fatal("setup: the request was not remembered")
	}
	limit := ctxMgr.ContextLimit()
	total := a.outgoingViewEstimate(a.rebuildView())
	if total < limit {
		t.Fatalf("setup: estimate %d under the window", total)
	}
	_, counts := a.messageCounts()
	for i, c := range counts {
		if c > 0 && total-c+lastResortCondenseAllowance < limit {
			t.Fatalf("setup: message %d replacement fits (%d - %d + %d < %d)", i, total, c, lastResortCondenseAllowance, limit)
		}
	}

	view, err := a.prepareMessages(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasContent(a.Messages, request) {
		t.Fatal("the remembered request stayed verbatim over the window")
	}
	if est := a.outgoingViewEstimate(view); est >= limit {
		t.Fatalf("estimate %d still over the window", est)
	}
}
