package agent

import (
	"testing"

	"gogen/internal/llm"
)

func TestMessagesEqualIgnoresReasoning(t *testing.T) {
	a := &llm.Message{Role: "assistant", Content: "hi", Reasoning: "think A"}
	b := &llm.Message{Role: "assistant", Content: "hi", Reasoning: "think B"}
	if !messagesEqual(a, b) {
		t.Fatal("Reasoning should not affect wire-view equality")
	}
}

func TestMessagesEqualDetectsArgsStrDrift(t *testing.T) {
	a := &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", ArgsStr: `{"path":"a"}`}}}
	b := &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", ArgsStr: `{"path":"b"}`}}}
	if messagesEqual(a, b) {
		t.Fatal("expected ArgsStr mismatch")
	}
}

func TestCloneViewMessagesDetachesToolCalls(t *testing.T) {
	orig := []llm.Message{{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:      "1",
			Name:    "read_file",
			ArgsStr: `{"path":"a"}`,
		}},
	}}
	cloned := cloneViewMessages(orig)
	orig[0].ToolCalls[0].ArgsStr = `{"path":"mutated"}`
	if cloned[0].ToolCalls[0].ArgsStr != `{"path":"a"}` {
		t.Fatalf("clone shares ToolCalls backing: %q", cloned[0].ToolCalls[0].ArgsStr)
	}
}

func TestCompareViewFingerprintsIgnoresAppendOnlyGrowth(t *testing.T) {
	a := &Agent{
		DebugCompareMessages: true,
		lastViewMessages: cloneViewMessages([]llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		}),
	}
	// Longer view with identical prefix must not panic and must not treat
	// append-only growth as cache-busting drift.
	current := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok"},
	}
	a.compareViewFingerprints(current) // must not panic
}

func TestStabilizeViewToolArgsPinsSharedBacking(t *testing.T) {
	msgs := []llm.Message{{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:   "1",
			Name: "read_file",
			Args: map[string]interface{}{"path": "a.go"},
		}},
	}}
	view := append([]llm.Message(nil), msgs...)
	stabilizeViewToolArgs(view)
	if msgs[0].ToolCalls[0].ArgsStr != `{"path":"a.go"}` {
		t.Fatalf("expected pin into shared ToolCalls, got %q", msgs[0].ToolCalls[0].ArgsStr)
	}
}
