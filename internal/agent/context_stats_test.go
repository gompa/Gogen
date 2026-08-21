package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestApplyAPIBaselineShrunkConversation pins the truncation guard: when
// the message list shrank below the recorded baseline (a rollback that did
// not clear the usage baseline), the stale API count must not be applied
// verbatim — the locally computed estimate stays.
func TestApplyAPIBaselineShrunkConversation(t *testing.T) {
	snap := contextmgr.ContextSnapshot{Used: 1234, Limit: 8000, Percent: 0.154}
	usage := &llm.Usage{PromptTokens: 5000, CompletionTokens: 100}
	msgs := []llm.Message{{Role: "user", Content: "hi"}}
	counts := []int{10}
	applyAPIBaseline(&snap, msgs, counts, usage, 5000, 4, true)
	if snap.Used != 1234 || snap.Percent != 0.154 {
		t.Fatalf("snapshot mutated to Used=%d Percent=%f; want the local estimate unchanged", snap.Used, snap.Percent)
	}

	// The growth direction is untouched: messages appended since the
	// baseline add their local estimates on top of the API count.
	snap2 := contextmgr.ContextSnapshot{Used: 0, Limit: 8000}
	msgs2 := []llm.Message{{Role: "user", Content: "a"}, {Role: "user", Content: "b"}}
	applyAPIBaseline(&snap2, msgs2, []int{10, 20}, usage, 5000, 1, true)
	if snap2.Used != 5020 {
		t.Fatalf("Used = %d, want 5000 (baseline) + 20 (appended)", snap2.Used)
	}
}

type statsStubProvider struct {
	limit        int
	models       []llm.ModelInfo
	streamResult *llm.StreamResult // optional override for GenerateResponseStream
}

func (s *statsStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "summary"}, nil
}

func (s *statsStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	if s.streamResult != nil {
		return s.streamResult, nil
	}
	return &llm.StreamResult{}, nil
}

func (s *statsStubProvider) ModelContextLimit(_ context.Context) (int, error) {
	return s.limit, nil
}

func (s *statsStubProvider) SetThinkingLevel(string) {}

func (s *statsStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	if s.models != nil {
		return s.models, nil
	}
	return nil, nil
}

func (s *statsStubProvider) SetModel(string) error { return nil }
func (s *statsStubProvider) ModelName() string     { return "test-model" }

// TestContextStatsUsesAPIBaseline verifies that when the API returned usage,
// ContextStats uses it as the authoritative PromptTokens baseline for
// Snapshot.Used rather than a full local estimate.
func TestContextStatsUsesAPIBaseline(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.Messages = []llm.Message{{Role: "user", Content: strings.Repeat("x", 4000)}}
	a.recordTurnUsage(&llm.Usage{PromptTokens: 900, CompletionTokens: 50, TotalTokens: 950, CachedTokens: 400})

	stats := a.ContextStats(context.Background())
	if stats.Snapshot.Used != 900 {
		t.Fatalf("Used should use API baseline (900) when messages haven't changed, got %d", stats.Snapshot.Used)
	}
	if stats.PromptTokens != 900 || stats.CompletionTokens != 50 || stats.CachedTokens != 400 {
		t.Fatalf("unexpected last turn usage: %+v", stats)
	}
}

// TestContextStatsAPIBaselineWithExtraMessages verifies that when messages
// are appended after the API baseline was recorded, Snapshot.Used equals
// the API's PromptTokens plus a local estimate for the new messages.
func TestContextStatsAPIBaselineWithExtraMessages(t *testing.T) {
	provider := &statsStubProvider{limit: 10000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 10000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.Messages = []llm.Message{
		{Role: "user", Content: "hello"},
	}
	// Simulate: API call happened with 1 message, returned 10 prompt tokens.
	a.recordTurnUsage(&llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})

	// Assistant response was appended after the API call (messages now = 2).
	a.Messages = append(a.Messages, llm.Message{Role: "assistant", Content: "world"})

	stats := a.ContextStats(context.Background())
	if stats.Snapshot.Used <= 10 {
		t.Fatalf("Used should be API baseline (10) + estimate for new message, got %d", stats.Snapshot.Used)
	}
	// The estimate for "world" should be > 0.
	if stats.PromptTokens != 10 || stats.CompletionTokens != 5 {
		t.Fatalf("unexpected last turn usage: prompt=%d completion=%d", stats.PromptTokens, stats.CompletionTokens)
	}

	// Verify /context detail view still works (it shows the API counters).
	out, handled := a.HandleContextCommand(context.Background(), "/context")
	if !handled {
		t.Fatal("expected handled")
	}
	t.Logf("/context output: %s", out)
}

func TestContextStatsDoesNotMutateMessages(t *testing.T) {
	provider := &statsStubProvider{limit: 200}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:       200,
		MaxToolResultBytes: 5,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	big := strings.Repeat("x", 4000)
	a.Messages = []llm.Message{{Role: "tool", Content: big, ToolCallID: "c1"}}
	_ = a.ContextStats(context.Background())
	if a.Messages[0].Content != big {
		t.Fatal("ContextStats must not mutate canonical tool results")
	}
}

func TestFormatContextBrief(t *testing.T) {
	line := FormatContextBrief(TurnContext{
		PromptTokens: 42300,
		CachedTokens: 30000,
		Snapshot: contextmgr.ContextSnapshot{
			Used:    42300,
			Limit:   128000,
			Percent: 42300.0 / 128000.0,
		},
	})
	if !strings.Contains(line, "42.3k / 128k") {
		t.Fatalf("unexpected line: %q", line)
	}
	if strings.Contains(line, "estimated") {
		t.Fatalf("brief should not include estimated suffix: %q", line)
	}
	if !strings.Contains(line, "30k cached") {
		t.Fatalf("expected cached tokens: %q", line)
	}
}

func TestRecordTurnUsageIgnoresNil(t *testing.T) {
	a := NewAgent(&statsStubProvider{limit: 1000}, &Executor{WorkingDir: "."}, nil)
	a.recordTurnUsage(&llm.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11})
	a.recordTurnUsage(nil)
	if a.lastTurnUsage == nil || a.lastTurnUsage.PromptTokens != 10 {
		t.Fatalf("nil usage cleared lastTurnUsage: %+v", a.lastTurnUsage)
	}
}

// TestContextStatsConcurrentWithTurn verifies ContextStats is safe to call
// while a turn goroutine appends messages, extends the cached token counts,
// records API usage, and stabilizes tool args in place. The web server calls
// ContextStats and SnapshotMessages without turnMu during a stream
// (connect-time and /models goroutines), so the shared state must be
// synchronized independently of the turn lock. Run with -race (make test)
// to catch regressions.
func TestContextStatsConcurrentWithTurn(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.Messages = []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	// Restore a shorter count cache (as after a session restore with new
	// messages) so appendMessage has to extend it while readers run.
	a.restoreMessages(a.Messages, []int{1, 2, 3, 4})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers: simulate web WS readers probing context stats mid-turn.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					stats := a.ContextStats(context.Background())
					if stats.Snapshot.MessageCount < 4 || stats.Snapshot.MessageCount > 4+200 {
						// The writer appends up to 200 messages; a torn or
						// overlapping snapshot outside this range would mean
						// the lock discipline is broken. Keep the read
						// observable to the race detector.
						t.Errorf("MessageCount=%d out of range", stats.Snapshot.MessageCount)
						return
					}
					if msgs := a.SnapshotMessages(); len(msgs) < 4 || len(msgs) > 4+200 {
						t.Errorf("SnapshotMessages len=%d out of range", len(msgs))
						return
					}
				}
			}
		}()
	}

	// Writer: simulate the turn goroutine appending messages (with tool calls
	// so the deep clone and in-place stabilization paths are exercised),
	// extending counts, and recording usage.
	for i := 0; i < 200; i++ {
		tc := []llm.ToolCall{{ID: fmt.Sprintf("call_%d", i), Name: "read_file", Args: map[string]interface{}{"path": "a.go"}}}
		a.appendMessage(llm.Message{Role: "assistant", Content: "resp", ToolCalls: tc})
		a.stabilizeToolArgs()
		a.recordTurnUsage(&llm.Usage{PromptTokens: 10 + i, CompletionTokens: 1, TotalTokens: 11 + i})
	}
	close(stop)
	wg.Wait()

	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	if len(a.tokenCounts) != len(a.Messages) {
		t.Fatalf("tokenCounts len=%d, want %d", len(a.tokenCounts), len(a.Messages))
	}
}

// TestTokenCountsCacheIncremental verifies the per-message token-count cache:
// appendMessage extends a complete cache, ContextStats backfills an empty or
// incomplete one, and wholesale replacements (compaction/restore/rollback)
// clear it so stale counts are never reused.
func TestTokenCountsCacheIncremental(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	// Fresh session: no cache until the first ContextStats backfills it.
	a.appendMessage(llm.Message{Role: "user", Content: "hello world"})
	a.statsMu.RLock()
	if a.tokenCounts != nil {
		t.Fatalf("expected nil cache before first ContextStats, got %d entries", len(a.tokenCounts))
	}
	a.statsMu.RUnlock()

	stats := a.ContextStats(context.Background())
	if stats.Snapshot.MessageCount != 1 {
		t.Fatalf("MessageCount=%d, want 1", stats.Snapshot.MessageCount)
	}
	a.statsMu.RLock()
	got := a.tokenCounts
	a.statsMu.RUnlock()
	if len(got) != 1 {
		t.Fatalf("cache not backfilled after ContextStats: len=%d", len(got))
	}
	want := contextmgr.ComputeMessageTokens(llm.Message{Role: "user", Content: "hello world"})
	if got[0] != want {
		t.Fatalf("cached count=%d, want %d", got[0], want)
	}

	// Appends extend a complete cache without invalidating earlier entries.
	a.appendMessage(llm.Message{Role: "assistant", Content: "hi"})
	a.statsMu.RLock()
	got = a.tokenCounts
	a.statsMu.RUnlock()
	if len(got) != 2 || got[0] != want {
		t.Fatalf("cache not extended on append: len=%d first=%d", len(got), got[0])
	}

	// A second ContextStats must be the fast path (cache already complete).
	if stats := a.ContextStats(context.Background()); stats.Snapshot.MessageCount != 2 {
		t.Fatalf("MessageCount=%d, want 2", stats.Snapshot.MessageCount)
	}

	// A wholesale replacement (compaction / fork / reset) clears the cache.
	a.replaceMessages([]llm.Message{{Role: "user", Content: "fresh"}})
	a.statsMu.RLock()
	got = a.tokenCounts
	a.statsMu.RUnlock()
	if got != nil {
		t.Fatalf("expected cache cleared after replaceMessages, got %d entries", len(got))
	}

	// Rollback (truncateMessages) trims a complete cache to match.
	a.appendMessage(llm.Message{Role: "user", Content: "q"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "a"})
	_ = a.ContextStats(context.Background()) // backfill
	a.truncateMessages(1)
	a.statsMu.RLock()
	got = a.tokenCounts
	n := len(a.Messages)
	a.statsMu.RUnlock()
	if len(got) != n {
		t.Fatalf("cache not trimmed after rollback: len=%d, messages=%d", len(got), n)
	}
}

// TestShouldCompactUsingCountsMatchesDirect verifies the cached-count
// compaction decision agrees with the full EstimateTokens pass. No API
// baseline is recorded here, so both sides add the wire overhead
// (system prompt + tool definitions) that the canonical messages omit.
func TestShouldCompactUsingCountsMatchesDirect(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              1000,
		CompactKeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	// Enough messages to exceed CompactKeepRecentMessages+1.
	for i := 0; i < 12; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 400)})
		a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 400)})
	}

	// Without a cache the helper must fall back to the direct computation.
	if got, want := a.shouldCompactUsingCounts(), a.Context.ShouldCompactWithOverhead(a.Messages, a.wireOverheadTokens()); got != want {
		t.Fatalf("fallback decision=%v, want %v", got, want)
	}

	// Once ContextStats fills the cache, the decisions must still agree.
	_ = a.ContextStats(context.Background())
	if got, want := a.shouldCompactUsingCounts(), a.Context.ShouldCompactWithOverhead(a.Messages, a.wireOverheadTokens()); got != want {
		t.Fatalf("cached decision=%v, want %v", got, want)
	}
}

func TestHandleContextCommand(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	out, handled := a.HandleContextCommand(context.Background(), "/context")
	if !handled {
		t.Fatal("expected handled")
	}
	if !strings.Contains(out, "Context (estimated)") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestCompactHistoryClearsLastTurnUsage verifies that after manual compaction
// the per-request API counters (frozen from the pre-compaction turn) are no
// longer reported by /context, since the history they describe was replaced.
func TestCompactHistoryClearsLastTurnUsage(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              1000,
		CompactKeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	// Enough messages to exceed CompactKeepRecentMessages+1 for compaction, sized so
	// the summarized middle clears the minimum-middle guard.
	for i := 0; i < 6; i++ {
		a.Messages = append(a.Messages,
			llm.Message{Role: "user", Content: "q " + strconv.Itoa(i) + " " + strings.Repeat("x", 300)},
			llm.Message{Role: "assistant", Content: "a " + strconv.Itoa(i) + " " + strings.Repeat("y", 300)},
		)
	}
	a.recordTurnUsage(&llm.Usage{PromptTokens: 900, CompletionTokens: 50, TotalTokens: 950})

	if err := a.CompactHistory(context.Background()); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	stats := a.ContextStats(context.Background())
	if stats.PromptTokens != 0 || stats.CompletionTokens != 0 || stats.CachedTokens != 0 {
		t.Fatalf("expected stale last-turn usage cleared after compaction, got %+v", stats)
	}
}

// TestSelectModelClearsLastTurnUsage verifies that switching models drops the
// previous request's API counters so /context does not show figures measured
// against the old model's context accounting.
func TestSelectModelClearsLastTurnUsage(t *testing.T) {
	provider := &statsStubProvider{limit: 1000, models: []llm.ModelInfo{{ID: "test-model"}}}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.Messages = []llm.Message{{Role: "user", Content: "hi"}}
	a.recordTurnUsage(&llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110})

	if err := a.SelectModel(context.Background(), "test-model"); err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	stats := a.ContextStats(context.Background())
	if stats.PromptTokens != 0 || stats.CompletionTokens != 0 {
		t.Fatalf("expected stale last-turn usage cleared after model switch, got %+v", stats)
	}
}
