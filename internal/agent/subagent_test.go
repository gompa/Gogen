package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// fakeSpawner records spawn requests and returns a canned report.
type fakeSpawner struct {
	calls int
	last  struct {
		parent *Agent
		job    string
		model  string
		depth  int
	}
	report string
	err    error
}

func (f *fakeSpawner) Spawn(ctx context.Context, parent *Agent, job, model string, depth int) (string, error) {
	f.calls++
	f.last.parent = parent
	f.last.job = job
	f.last.model = model
	f.last.depth = depth
	return f.report, f.err
}

func newSubagentTestAgent(t *testing.T) *Agent {
	t.Helper()
	prov := llm.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	return NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
}

// TestSubagentToolGating verifies the MCP-style gating: no trace when
// disabled; exposed and callable when enabled; plan mode blocks it.
func TestSubagentToolGating(t *testing.T) {
	a := newSubagentTestAgent(t)

	// Disabled: absent from every surface, "unknown tool" on call.
	for _, def := range a.llmTools() {
		if def.Name == "subagent" {
			t.Fatal("subagent tool must not be exposed when disabled")
		}
	}
	if _, ok := a.AllowedToolNames()["subagent"]; ok {
		t.Fatal("subagent must not be allowed when disabled")
	}
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{"job": "x"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool, got %v", err)
	}

	// Enabled + spawner: exposed and callable.
	sp := &fakeSpawner{report: "report: done"}
	a.SetSubagentsEnabled(true)
	a.SetSubagentSpawner(sp)
	found := false
	for _, def := range a.llmTools() {
		if def.Name == "subagent" {
			found = true
		}
	}
	if !found {
		t.Fatal("subagent tool should be exposed when enabled")
	}
	if _, ok := a.AllowedToolNames()["subagent"]; !ok {
		t.Fatal("subagent should be allowed when enabled")
	}
	out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{"job": "do it"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "report: done" {
		t.Fatalf("report = %q", out)
	}
	if sp.calls != 1 || sp.last.job != "do it" || sp.last.depth != 0 || sp.last.parent != a {
		t.Fatalf("spawn call = %+v", sp.last)
	}

	// Plan mode blocks the subagent tool (unlike the board).
	a.SetMode(ModePlan)
	if _, ok := a.AllowedToolNames()["subagent"]; ok {
		t.Fatal("subagent must be blocked in plan mode")
	}
	if err := a.checkPlanMode("subagent"); err == nil {
		t.Fatal("plan mode must block subagent")
	}
}

// TestSubagentSpawnerNil verifies the nil-guard: enabled but no spawner
// installed → a clear error, not a panic.
func TestSubagentSpawnerNil(t *testing.T) {
	a := newSubagentTestAgent(t)
	a.SetSubagentsEnabled(true)
	_, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{"job": "x"}})
	if err == nil || !strings.Contains(err.Error(), "spawner is not installed") {
		t.Fatalf("expected spawner-not-installed error, got %v", err)
	}
}

// TestSubagentDepthLimit verifies the default depth of 1: a child (depth 1)
// cannot spawn; raising subagent_max_depth re-enables nesting.
func TestSubagentDepthLimit(t *testing.T) {
	parent := newSubagentTestAgent(t)
	parent.SetSubagentsEnabled(true)
	sp := &fakeSpawner{report: "ok"}
	parent.SetSubagentSpawner(sp)

	// Parent (depth 0) spawns fine.
	if _, err := parent.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{"job": "parent job"}}); err != nil {
		t.Fatal(err)
	}

	// Child (depth 1) with the default max depth 1 is blocked BEFORE the
	// spawner runs.
	child := newSubagentTestAgent(t)
	child.SetSubagentsEnabled(true)
	child.SetSubagentSpawner(sp)
	child.SetSubagentDepth(1)
	_, err := child.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{"job": "child job"}})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("expected depth-limit error, got %v", err)
	}
	if sp.calls != 1 {
		t.Fatalf("spawner ran for a depth-limited call (%d calls)", sp.calls)
	}

	// Raising the limit re-enables nesting.
	child.SetSubagentMaxDepth(3)
	if _, err := child.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{"job": "child job"}}); err != nil {
		t.Fatal(err)
	}
	if sp.calls != 2 || sp.last.depth != 1 {
		t.Fatalf("spawn call = %+v", sp.last)
	}
}

// TestSubagentToolRequiresJob verifies the required job argument.
func TestSubagentToolRequiresJob(t *testing.T) {
	a := newSubagentTestAgent(t)
	a.SetSubagentsEnabled(true)
	a.SetSubagentSpawner(&fakeSpawner{report: "ok"})
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]interface{}{}}); err == nil {
		t.Fatal("missing job should fail")
	}
}
