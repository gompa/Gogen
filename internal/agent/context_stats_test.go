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

func TestContextStatsUsesAPIUsage(t *testing.T) {
	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.Messages = []llm.Message{{Role: "user", Content: strings.Repeat("x", 4000)}}
	a.recordTurnUsage(&llm.Usage{PromptTokens: 900, CompletionTokens: 50, TotalTokens: 950})

	stats := a.ContextStats(context.Background())
	if stats.UsedSource != contextSourceAPI {
		t.Fatalf("got source %q", stats.UsedSource)
	}
	if stats.Snapshot.Used != 900 {
		t.Fatalf("got used %d", stats.Snapshot.Used)
	}
	if stats.PromptTokens != 900 || stats.CompletionTokens != 50 {
		t.Fatalf("unexpected last turn usage: %+v", stats)
	}
}

func TestFormatContextBrief(t *testing.T) {
	line := FormatContextBrief(TurnContext{
		UsedSource: contextSourceAPI,
		PromptTokens: 42300,
		Snapshot: contextmgr.ContextSnapshot{
			Used:    42300,
			Limit:   128000,
			Percent: 42300.0 / 128000.0,
		},
	})
	if !strings.Contains(line, "42.3k / 128k") {
		t.Fatalf("unexpected line: %q", line)
	}
	if !strings.Contains(line, "last request") {
		t.Fatalf("expected source label: %q", line)
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
