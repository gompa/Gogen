package agent

import (
	"context"
	"testing"

	"gogen/internal/llm"
)

func TestRepairOrphanToolCallsAppendsPlaceholders(t *testing.T) {
	a := &Agent{
		Messages: []llm.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file"},
				{ID: "call_2", Name: "search_code"},
			}},
		},
	}
	if !a.RepairOrphanToolCalls() {
		t.Fatal("expected repair to modify messages")
	}
	if len(a.Messages) != 4 {
		t.Fatalf("got %d messages, want 4", len(a.Messages))
	}
	if a.Messages[2].Role != "tool" || a.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("msg[2] = %#v", a.Messages[2])
	}
	if a.Messages[3].Role != "tool" || a.Messages[3].ToolCallID != "call_2" {
		t.Fatalf("msg[3] = %#v", a.Messages[3])
	}
	if a.Messages[2].Content == "" || a.Messages[3].Content == "" {
		t.Fatal("expected cancelled placeholder content")
	}
	if a.RepairOrphanToolCalls() {
		t.Fatal("second repair should be a no-op")
	}
}

func TestRepairOrphanToolCallsPartialResults(t *testing.T) {
	a := &Agent{
		Messages: []llm.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file"},
				{ID: "call_2", Name: "search_code"},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "ok"},
		},
	}
	if !a.RepairOrphanToolCalls() {
		t.Fatal("expected repair for missing call_2")
	}
	if len(a.Messages) != 4 {
		t.Fatalf("got %d messages, want 4", len(a.Messages))
	}
	if a.Messages[3].ToolCallID != "call_2" {
		t.Fatalf("msg[3] = %#v", a.Messages[3])
	}
}

func TestRepairOrphanToolCallsNoOpWhenComplete(t *testing.T) {
	a := &Agent{
		Messages: []llm.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "read_file"},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "ok"},
		},
	}
	if a.RepairOrphanToolCalls() {
		t.Fatal("complete protocol should not repair")
	}
}

func TestContextStatsCancelledSkipsWork(t *testing.T) {
	a := &Agent{
		Messages: []llm.Message{{Role: "user", Content: "x"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats := a.ContextStats(ctx)
	if stats.Snapshot.MessageCount != 1 {
		t.Fatalf("MessageCount = %d, want 1", stats.Snapshot.MessageCount)
	}
}
