package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// overflowProvider is a stub provider whose streaming call fails with a
// context-window refusal overflowErrs times before succeeding with a plain
// content result. err, when set, is returned on EVERY stream call instead
// (the fixed-error paths). cancel, when set, is called on the first stream
// call (the cancelled-context path). GenerateResponse serves the compaction
// summarization call and counts the calls.
type overflowProvider struct {
	overflowErrs int32
	err          error
	cancel       func()
	failSummary  bool
	streamCalls  int64
	summCalls    int64
}

func (p *overflowProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	atomic.AddInt64(&p.summCalls, 1)
	if p.failSummary {
		return llm.Response{}, errors.New("summarization unavailable")
	}
	return llm.Response{Content: "summary"}, nil
}

func (p *overflowProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	if atomic.AddInt64(&p.streamCalls, 1) == 1 && p.cancel != nil {
		p.cancel()
	}
	if p.err != nil {
		return nil, p.err
	}
	if atomic.AddInt32(&p.overflowErrs, -1) >= 0 {
		return nil, llm.ErrContextWindowExceeded
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (p *overflowProvider) ModelContextLimit(_ context.Context) (int, error) { return 1000000, nil }
func (p *overflowProvider) SetThinkingLevel(string)                          {}
func (p *overflowProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *overflowProvider) SetModel(string) error { return nil }
func (p *overflowProvider) ModelName() string     { return "test-model" }

// newOverflowTestAgent builds an agent with a generous context window (the
// local estimates say the request FITS — that is the premise of Phase 3:
// the provider is the source of truth and refuses anyway).
func newOverflowTestAgent(t *testing.T, p *overflowProvider) (*Agent, *contextmgr.Manager) {
	t.Helper()
	mgr := contextmgr.NewManager(p, contextmgr.Settings{
		ContextLimit:              1000000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	return NewAgent(p, &Executor{WorkingDir: "."}, mgr), mgr
}

// seedOverflowConversation builds a 6-message history with a pinned
// mid-history tool group, fills the per-message count cache, and returns
// the pin manager. Layout:
//
//	0: user "hello" (head)
//	1: user large
//	2: user large
//	3: assistant tool_call call_1
//	4: tool call_1 result (pinned)
//	5: user large
func seedOverflowConversation(t *testing.T, a *Agent) *PinManager {
	t.Helper()
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 400)})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 400)})
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "search_code", Args: map[string]any{"pattern": "x"}}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "3 hits in main.go"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 400)})
	_ = a.ContextStats(context.Background())
	pm := NewPinManager()
	a.PinManager = pm
	pm.ReplacePins(map[int]struct{}{4: {}}) // pin the tool result
	return pm
}

// TestOverflowRecoveryRetriesAfterForcedCompaction pins the core fix: the
// provider refuses the request once with a context-window error (the local
// estimate said it fit), the run loop recovers in-loop — forced compaction
// (shrunken history, pin remap) — and retries the round, which succeeds.
func TestOverflowRecoveryRetriesAfterForcedCompaction(t *testing.T) {
	provider := &overflowProvider{overflowErrs: 1}
	a, _ := newOverflowTestAgent(t, provider)
	seedOverflowConversation(t, a)
	var compacting int32
	h := &llm.StreamHandlers{OnCompacting: func() { atomic.AddInt32(&compacting, 1) }}

	out, err := a.StreamProcessInput(context.Background(), "next task", h)
	if err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	if out != "done" {
		t.Fatalf("out = %q, want %q", out, "done")
	}
	if got := atomic.LoadInt64(&provider.streamCalls); got != 2 {
		t.Fatalf("stream calls = %d, want 2 (one refusal + one retry)", got)
	}
	if got := atomic.LoadInt64(&provider.summCalls); got == 0 {
		t.Fatal("no summarization calls: the forced compaction did not run")
	}
	if got := atomic.LoadInt32(&compacting); got == 0 {
		t.Fatal("OnCompacting never fired")
	}
	// Shrunken history: 7 messages (6 seeded + the new user prompt) ->
	// head + summary + pinned tail (assistant call + tool result + the new
	// prompt) + the retried assistant reply.
	if len(a.Messages) != 7 {
		t.Fatalf("messages = %d, want 7 (head + summary + tail 3 + reply)", len(a.Messages))
	}
	if a.Messages[0].Content != "hello" {
		t.Fatalf("head = %q, want the starting prompt preserved verbatim", a.Messages[0].Content)
	}
	if sum := a.Messages[1]; sum.Role != "assistant" || len(sum.ToolCalls) != 0 || !contextmgr.IsCompactionSummary(sum.Content) {
		t.Fatalf("second message = role %q, %d tool calls, summary=%v; want a plain assistant summary",
			sum.Role, len(sum.ToolCalls), contextmgr.IsCompactionSummary(sum.Content))
	}
	// The pinned tool group survived verbatim and the tail starts at the
	// CALLING assistant message (walk-back over the trailing tool result).
	if m := a.Messages[2]; m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tail start = role %q (tool calls %d), want the calling assistant message", m.Role, len(m.ToolCalls))
	}
	if m := a.Messages[3]; m.Role != "tool" || m.ToolCallID != "call_1" {
		t.Fatalf("tail = role %q id %q, want the pinned tool result", m.Role, m.ToolCallID)
	}
	// Pin remap: the pinned tool result moved from index 4 to index 3.
	if pins := a.PinManager.PinnedSet(); len(pins) != 1 {
		t.Fatalf("pins after compaction = %v, want exactly the remapped pin", pins)
	} else if _, ok := pins[3]; !ok {
		t.Fatalf("pins after compaction = %v, want index 3 (remapped from 4)", pins)
	}
	// The outgoing view keeps a valid tool protocol.
	assertToolProtocol(t, a.rebuildView())
	if m := a.Messages[len(a.Messages)-1]; m.Role != "assistant" || m.Content != "done" {
		t.Fatalf("last message = %+v, want the retried assistant reply", m)
	}
}

// TestOverflowSecondRefusalNoRetry pins the terminal path: after the forced
// compaction already ran once this turn, a second refusal does not retry —
// the turn ends with an actionable error that still carries the original
// refusal in its chain (never masked).
func TestOverflowSecondRefusalNoRetry(t *testing.T) {
	provider := &overflowProvider{overflowErrs: 2}
	a, mgr := newOverflowTestAgent(t, provider)
	seedOverflowConversation(t, a)

	_, err := a.StreamProcessInput(context.Background(), "next task", nil)
	if err == nil {
		t.Fatal("expected the terminal error, got nil")
	}
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("terminal error lost the original refusal: %v", err)
	}
	if got := atomic.LoadInt64(&provider.streamCalls); got != 2 {
		t.Fatalf("stream calls = %d, want 2 (no retry after the second refusal)", got)
	}
	if got := atomic.LoadInt64(&provider.summCalls); got != 1 {
		t.Fatalf("summarization calls = %d, want 1 (one forced compaction per turn)", got)
	}
	msg := err.Error()
	for _, want := range []string{"context window exceeded", "/compact", "/new", "model window"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("terminal error missing %q: %v", want, err)
		}
	}
	if !strings.Contains(msg, strconv.Itoa(mgr.ContextLimit())) {
		t.Fatalf("terminal error missing the window size %d: %v", mgr.ContextLimit(), err)
	}
}

// TestOverflowCancelledContextNoRetry pins the cancellation guard: a
// context-window refusal that arrives together with a cancelled context is
// NOT retried — the turn ends with the cancellation, no compaction runs.
func TestOverflowCancelledContextNoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &overflowProvider{overflowErrs: 1, cancel: cancel}
	a, _ := newOverflowTestAgent(t, provider)
	seedOverflowConversation(t, a)

	_, err := a.StreamProcessInput(ctx, "next task", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt64(&provider.streamCalls); got != 1 {
		t.Fatalf("stream calls = %d, want 1 (no retry on cancellation)", got)
	}
	if got := atomic.LoadInt64(&provider.summCalls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 (no compaction on cancellation)", got)
	}
}

// TestOverflowNonOverflowErrorNoRetry pins the regression guard: a
// non-overflow provider error takes the untouched err != nil path — no
// retry, no compaction, the original error surfaces unchanged.
func TestOverflowNonOverflowErrorNoRetry(t *testing.T) {
	boom := errors.New("internal server error (500)")
	provider := &overflowProvider{err: boom}
	a, _ := newOverflowTestAgent(t, provider)
	seedOverflowConversation(t, a)
	before := len(a.Messages)

	_, err := a.StreamProcessInput(context.Background(), "next task", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original error", err)
	}
	if got := atomic.LoadInt64(&provider.streamCalls); got != 1 {
		t.Fatalf("stream calls = %d, want 1 (no retry for non-overflow errors)", got)
	}
	if got := atomic.LoadInt64(&provider.summCalls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 (no compaction for non-overflow errors)", got)
	}
	if len(a.Messages) != before+1 {
		t.Fatalf("messages = %d, want %d (history untouched)", len(a.Messages), before+1)
	}
}

// TestOverflowNonOverflowErrorCancelledContextNotMasked pins the error
// ordering in handleOverflowError: a NON-overflow error that arrives
// together with a cancelled context must still surface the original error
// — the cancellation must not mask it (the ctx.Err() short-circuit applies
// to context-window refusals only).
func TestOverflowNonOverflowErrorCancelledContextNotMasked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	boom := errors.New("internal server error (500)")
	provider := &overflowProvider{err: boom, cancel: cancel}
	a, _ := newOverflowTestAgent(t, provider)
	seedOverflowConversation(t, a)

	_, err := a.StreamProcessInput(ctx, "next task", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original non-overflow error (not masked by the cancellation)", err)
	}
	if got := atomic.LoadInt64(&provider.streamCalls); got != 1 {
		t.Fatalf("stream calls = %d, want 1 (no retry)", got)
	}
	if got := atomic.LoadInt64(&provider.summCalls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 (no compaction)", got)
	}
}

// TestOverflowCompactionFailureTerminalError pins the forced-compaction
// failure path: the refusal is real but the summarization call fails — the
// turn ends with the actionable terminal error wrapping the ORIGINAL
// refusal (the compaction failure is a separate concern and must not mask
// it).
func TestOverflowCompactionFailureTerminalError(t *testing.T) {
	provider := &overflowProvider{overflowErrs: 1, failSummary: true}
	a, _ := newOverflowTestAgent(t, provider)
	seedOverflowConversation(t, a)

	_, err := a.StreamProcessInput(context.Background(), "next task", nil)
	if err == nil {
		t.Fatal("expected the terminal error, got nil")
	}
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("terminal error lost the original refusal: %v", err)
	}
	if got := atomic.LoadInt64(&provider.streamCalls); got != 1 {
		t.Fatalf("stream calls = %d, want 1 (no retry after a failed compaction)", got)
	}
	if got := atomic.LoadInt64(&provider.summCalls); got == 0 {
		t.Fatal("no summarization calls: the forced compaction did not run")
	}
	if !strings.Contains(err.Error(), "context window exceeded") {
		t.Fatalf("terminal error not actionable: %v", err)
	}
}
