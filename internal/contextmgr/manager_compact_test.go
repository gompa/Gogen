package contextmgr

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestAdjustCompactTailStartIncludesToolCallAssistant(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", Content: "file body", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "next"},
	}
	got := adjustCompactTailStart(messages, 2)
	if got != 1 {
		t.Fatalf("expected tail to include assistant tool call at index 1, got %d", got)
	}
}

func TestRenderMessagesForSummaryIncludesToolName(t *testing.T) {
	messages := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "search_code"}}},
		{Role: "tool", Content: "main.go:1:needle", ToolCallID: "c1"},
	}
	text := renderMessagesForSummary(messages, 8192)
	if !strings.Contains(text, "TOOL RESULT (search_code (c1)):") {
		t.Fatalf("expected tool name in summary, got %q", text)
	}
}

func TestCompactKeepsToolCallPairInTail(t *testing.T) {
	provider := &stubProvider{summary: "summary"}
	m := NewManager(provider, Settings{KeepRecentMessages: 3})
	msgs := []llm.Message{
		{Role: "user", Content: "fix auth"},
		{Role: "assistant", Content: "reading"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", Content: "file contents", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "add tests"},
	}
	out, err := m.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) == 0 {
		t.Fatalf("expected assistant tool call preserved in tail, got %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "c1" {
		t.Fatalf("expected tool result preserved in tail, got %+v", out[3])
	}
}
