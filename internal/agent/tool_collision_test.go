package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestMCPToolShadowsFeatureTool pins the name-collision precedence: a
// registered MCP tool named "board" (or "subagent") wins over the feature
// tool — at EXECUTION (executeTool prefers the registry) AND in the
// model-facing definition list (llmTools includes the MCP def and skips the
// feature def), so what the model sees and what executes always agree and
// the model never sees duplicate definitions for one name.
func TestMCPToolShadowsFeatureTool(t *testing.T) {
	dir := t.TempDir()
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetBoardEnabled(true)
	a.SetBoardManager(NewBoardManager(dir, false))
	a.SetSubagentsEnabled(true)
	a.SetSubagentSpawner(&fakeSpawner{report: "spawned"})
	mcp := &fakeMCPRegistry{
		names: map[string]struct{}{"board": {}, "subagent": {}, "read_file": {}},
		defs: []llm.Tool{
			{Name: "board", Description: "mcp board"},
			{Name: "subagent", Description: "mcp subagent"},
			{Name: "read_file", Description: "mcp read_file"},
		},
	}
	a.SetMCPRegistry(mcp)

	// The model-facing list: MCP definitions win, no duplicates.
	names := map[string]int{}
	for _, def := range a.llmTools() {
		names[def.Name]++
	}
	if names["board"] != 1 || names["subagent"] != 1 || names["read_file"] != 1 {
		t.Fatalf("duplicate or missing tool defs after MCP collision: %v", names)
	}
	for _, def := range a.llmTools() {
		if def.Name == "board" && !strings.Contains(def.Description, "mcp board") {
			t.Fatalf("board def = %q, want the MCP definition to win", def.Description)
		}
	}

	// Execution: the MCP handler runs (fake returns ""), not the board
	// tool's handler (which would return a board listing).
	out, err := a.executeTool(context.Background(), llm.ToolCall{ID: "t1", Name: "board", Args: map[string]interface{}{"action": "list"}})
	if err != nil {
		t.Fatalf("executeTool(board): %v", err)
	}
	if out != "" {
		t.Fatalf("board tool executed the feature handler (out %q), want the MCP handler", out)
	}
	if _, err := a.executeTool(context.Background(), llm.ToolCall{ID: "t2", Name: "read_file", Args: map[string]interface{}{"path": "x"}}); err != nil {
		t.Fatalf("MCP read_file should still resolve: %v", err)
	}
}
