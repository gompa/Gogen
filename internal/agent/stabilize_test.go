package agent

import (
	"testing"

	"gogen/internal/llm"
)

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
