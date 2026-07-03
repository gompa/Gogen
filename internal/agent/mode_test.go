package agent

import (
	"context"
	"testing"

	"gogen/internal/llm"
)

func TestPlanModeAllowedTools(t *testing.T) {
	a := &Agent{Mode: ModePlan}
	allowed := a.AllowedToolNames()
	if len(allowed) != 25 {
		t.Fatalf("expected 25 tools, got %d", len(allowed))
	}
	if _, ok := allowed["read_file"]; !ok {
		t.Fatal("read_file should be allowed")
	}
	if _, ok := allowed["git_status"]; !ok {
		t.Fatal("git_status should be allowed in plan mode")
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
	if _, ok := allowed["find_definition"]; !ok {
		t.Fatal("find_definition should be allowed in plan mode")
	}
	if _, ok := allowed["todo_list"]; !ok {
		t.Fatal("todo_list should be allowed in plan mode")
	}
	if _, ok := allowed["write_file"]; ok {
		t.Fatal("write_file should not be allowed")
	}
	if _, ok := allowed["run_lint"]; ok {
		t.Fatal("run_lint should not be allowed in plan mode")
	}
	if _, ok := allowed["move_file"]; ok {
		t.Fatal("move_file should not be allowed in plan mode")
	}
	if _, ok := allowed["git_commit"]; ok {
		t.Fatal("git_commit should not be allowed in plan mode")
	}
	if _, ok := allowed["todo_done"]; ok {
		t.Fatal("todo_done should not be allowed in plan mode")
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
