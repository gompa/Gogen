package agent

import (
	"context"
	"testing"

	"gogen/internal/llm"
)

func TestPlanModeAllowedTools(t *testing.T) {
	a := &Agent{Mode: ModePlan}
	allowed := a.AllowedToolNames()
	if len(allowed) != 17 {
		t.Fatalf("expected 17 tools, got %d", len(allowed))
	}
	if _, ok := allowed["read_file"]; !ok {
		t.Fatal("read_file should be allowed")
	}
	if _, ok := allowed["git"]; !ok {
		t.Fatal("git should be allowed in plan mode")
	}
	if _, ok := allowed["web_search"]; !ok {
		t.Fatal("web_search should be allowed in plan mode")
	}
	if _, ok := allowed["web_fetch"]; !ok {
		t.Fatal("web_fetch should be allowed in plan mode")
	}
	if _, ok := allowed["find_file"]; !ok {
		t.Fatal("find_file should be allowed in plan mode")
	}
	if _, ok := allowed["find_symbol"]; !ok {
		t.Fatal("find_symbol should be allowed in plan mode")
	}
	if _, ok := allowed["call_graph"]; !ok {
		t.Fatal("call_graph should be allowed in plan mode (read-only analysis)")
	}
	if _, ok := allowed["todo"]; !ok {
		t.Fatal("todo should be allowed in plan mode")
	}
	if _, ok := allowed["write_file"]; ok {
		t.Fatal("write_file should not be allowed")
	}
	if _, ok := allowed["git_commit"]; ok {
		t.Fatal("git_commit should not be allowed in plan mode")
	}
}

func TestPlanModeBlocksExecute(t *testing.T) {
	a := &Agent{Mode: ModePlan, Executor: &Executor{WorkingDir: t.TempDir()}}
	_, err := a.executeTool(context.Background(), llmToolCall("execute_command", map[string]interface{}{"command": "echo hi"}))
	if err == nil {
		t.Fatal("expected error")
	}
}

func llmToolCall(name string, args map[string]interface{}) llm.ToolCall {
	return llm.ToolCall{Name: name, Args: args}
}
