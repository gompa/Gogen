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

// keep_recent_messages 0 is literal: compaction keeps only the first user
// message and summarizes everything else (no tail).
func TestCompactKeepZeroKeepsOnlyHead(t *testing.T) {
	provider := &stubProvider{summary: "summary"}
	m := NewManager(provider, Settings{KeepRecentMessages: 0})
	msgs := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "b"},
		{Role: "assistant", Content: "c"},
	}
	out, err := m.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected [head, summary], got %d messages: %+v", len(out), out)
	}
	if out[0].Role != "user" || out[0].Content != "task" {
		t.Fatalf("expected first user message preserved, got %+v", out[0])
	}
	if !strings.Contains(out[1].Content, "summary") {
		t.Fatalf("expected summarized middle, got %+v", out[1])
	}
}

// keep=0 is literal, not "disable compaction": the len <= keep+1 guard only
// protects a head-only conversation. Once there is anything beyond the head,
// auto-compaction proceeds normally (and would summarize everything but the
// head, see TestCompactKeepZeroKeepsOnlyHead).
func TestShouldCompactWithKeepZero(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{KeepRecentMessages: 0, CompactThreshold: 0.5, ContextLimit: 8000})
	msgs := []llm.Message{
		{Role: "user", Content: strings.Repeat("word ", 20000)},
	}
	if m.ShouldCompact(msgs) {
		t.Fatal("expected no compaction for a head-only conversation (len <= keep+1)")
	}
	msgs = append(msgs, llm.Message{Role: "assistant", Content: strings.Repeat("word ", 20000)})
	if !m.ShouldCompact(msgs) {
		t.Fatal("expected compaction once there is something beyond the head")
	}
}

func TestSnapshotMarksCompactDisabled(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{CompactThreshold: 0, ContextLimit: 8000})
	snap := m.Snapshot(nil, nil)
	if !snap.CompactDisabled {
		t.Fatal("expected CompactDisabled for threshold 0")
	}
	if snap.NearCompact {
		t.Fatal("expected NearCompact false when auto-compaction is disabled")
	}
}

func TestSnapshotAutoCompactEnabledByDefault(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{CompactThreshold: 0.5, ContextLimit: 8000})
	snap := m.Snapshot(nil, nil)
	if snap.CompactDisabled {
		t.Fatal("expected auto-compaction enabled at threshold 0.5")
	}
}
