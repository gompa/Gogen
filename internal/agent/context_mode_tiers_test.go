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
