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
	total := a.outgoingViewEstimate(a.rebuildView())
	if total >= ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate is over the window")
	}
	if !condensableTailFits(a, total, ctxMgr.ContextLimit()) {
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
	largest, largestTok := largestCondensableFrom(msgs, counts, 0)
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
