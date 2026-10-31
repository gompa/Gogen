package agent

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

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
