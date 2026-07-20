package server

import (
	"testing"

	"gogen/internal/llm"
)

func TestHistoryEntriesIncludesTools(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "fix it"},
		{
			Role:    "assistant",
			Content: "I'll patch",
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "patch_file", Args: map[string]interface{}{"diff": "@@ -1 +1 @@\n-a\n+b\n"}},
			},
		},
		{Role: "tool", ToolCallID: "c1", Content: "Applied patch to 1 file"},
		{Role: "assistant", Content: "Done"},
	}
	got := historyEntries(msgs)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4: %#v", len(got), got)
	}
	if got[1].Role != "assistant" || len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Name != "patch_file" {
		t.Fatalf("assistant toolCalls: %#v", got[1])
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "c1" {
		t.Fatalf("tool entry: %#v", got[2])
	}
}

func TestHistoryEntriesIncludesReasoning(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "explain"},
		{
			Role:      "assistant",
			Content:   "The answer is 42",
			Reasoning: "Let me think about this...",
		},
		{
			Role:      "assistant",
			Content:   "",
			Reasoning: "Only reasoning, no content",
		},
		{
			Role:      "assistant",
			Content:   "Just content",
			Reasoning: "",
		},
	}
	got := historyEntries(msgs)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4: %#v", len(got), got)
	}
	if got[1].Reasoning != "Let me think about this..." {
		t.Fatalf("reasoning = %q", got[1].Reasoning)
	}
	if got[2].Reasoning != "Only reasoning, no content" {
		t.Fatalf("reasoning-only entry: %#v", got[2])
	}
	if got[3].Reasoning != "" {
		t.Fatalf("content-only entry should have empty reasoning: %q", got[3].Reasoning)
	}
}
