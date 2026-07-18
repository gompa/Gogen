package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type statsStubProvider struct {
	limit  int
	models []llm.ModelInfo
}

func (s *statsStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "summary"}, nil
}

func (s *statsStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (s *statsStubProvider) ModelContextLimit(_ context.Context) (int, error) {
	return s.limit, nil
}

func (s *statsStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	if s.models != nil {
		return s.models, nil
	}
	return nil, nil
}

func (s *statsStubProvider) SetModel(string) error { return nil }
func (s *statsStubProvider) ModelName() string     { return "test-model" }

func TestContextStatsUsesEstimatedHistory(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.Messages = []llm.Message{{Role: "user", Content: strings.Repeat("x", 4000)}}
	a.recordTurnUsage(&llm.Usage{PromptTokens: 900, CompletionTokens: 50, TotalTokens: 950, CachedTokens: 400})

	stats := a.ContextStats(context.Background())
	if stats.Snapshot.Used == 900 {
		t.Fatal("Used should track estimated history, not frozen API prompt tokens")
	}
	if stats.Snapshot.Used <= 0 {
		t.Fatalf("expected estimated used > 0, got %d", stats.Snapshot.Used)
	}
	if stats.PromptTokens != 900 || stats.CompletionTokens != 50 || stats.CachedTokens != 400 {
		t.Fatalf("unexpected last turn usage: %+v", stats)
	}
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
		ContextLimit:       1000,
		KeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	// Enough messages to exceed KeepRecentMessages+1 for compaction.
	for i := 0; i < 6; i++ {
		a.Messages = append(a.Messages,
			llm.Message{Role: "user", Content: "q " + strconv.Itoa(i)},
			llm.Message{Role: "assistant", Content: "a " + strconv.Itoa(i)},
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
