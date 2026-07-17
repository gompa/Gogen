package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type statsStubProvider struct {
	limit int
}

func (s *statsStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}

func (s *statsStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (s *statsStubProvider) ModelContextLimit(_ context.Context) (int, error) {
	return s.limit, nil
}

func (s *statsStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
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
