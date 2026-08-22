package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gogen/internal/config"
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
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "x"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
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
	out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "do it"}})
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

// TestSubagentToolDescriptionConcurrentLimit verifies the subagent tool
// description surfaces the live per-parent concurrent-subagent limit,
// defaulting to the config default.
func TestSubagentToolDescriptionConcurrentLimit(t *testing.T) {
	a := newSubagentTestAgent(t)
	a.SetSubagentsEnabled(true)
	a.SetSubagentSpawner(&fakeSpawner{})

	def := subagentToolDef(false, a.SubagentMaxConcurrent())
	want := fmt.Sprintf("At most %d may run concurrently.", config.DefaultSubagentMaxConcurrent)
	if !strings.Contains(def.Description, want) {
		t.Fatalf("description missing default concurrent limit:\n%s", def.Description)
	}

	a.SetSubagentMaxConcurrent(2)
	def = subagentToolDef(true, a.SubagentMaxConcurrent())
	if !strings.Contains(def.Description, "At most 2 may run concurrently.") {
		t.Fatalf("description missing live concurrent limit:\n%s", def.Description)
	}
}

// TestSubagentSpawnerNil verifies the nil-guard: enabled but no spawner
// installed → a clear error, not a panic.
func TestSubagentSpawnerNil(t *testing.T) {
	a := newSubagentTestAgent(t)
	a.SetSubagentsEnabled(true)
	_, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "x"}})
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
	if _, err := parent.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "parent job"}}); err != nil {
		t.Fatal(err)
	}

	// Child (depth 1) with the default max depth 1 is blocked BEFORE the
	// spawner runs.
	child := newSubagentTestAgent(t)
	child.SetSubagentsEnabled(true)
	child.SetSubagentSpawner(sp)
	child.SetSubagentDepth(1)
	_, err := child.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "child job"}})
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("expected depth-limit error, got %v", err)
	}
	if sp.calls != 1 {
		t.Fatalf("spawner ran for a depth-limited call (%d calls)", sp.calls)
	}

	// Raising the limit re-enables nesting.
	child.SetSubagentMaxDepth(3)
	if _, err := child.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "child job"}}); err != nil {
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
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{}}); err == nil {
		t.Fatal("missing job should fail")
	}
}

// TestSubagentLabelTruncatesRuneSafe pins the sidebar-label truncation: a
// job whose first line exceeds 60 RUNES is cut at the rune boundary, never
// mid-character (a byte cut would emit invalid UTF-8 into the label).
func TestSubagentLabelTruncatesRuneSafe(t *testing.T) {
	job := ""
	for i := 0; i < 65; i++ {
		job += "é"
	}
	got := SubagentLabel(job)
	want := "subagent: " + string([]rune(job)[:60]) + "…"
	if got != want {
		t.Fatalf("SubagentLabel = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("SubagentLabel is not valid UTF-8: %q", got)
	}
	// Short labels are untouched.
	if short := SubagentLabel("short job"); short != "subagent: short job" {
		t.Fatalf("SubagentLabel(short) = %q", short)
	}
}

// TestTruncateSubagentReportRuneSafe verifies the report cap never splits a
// multi-byte rune at the cut point (a byte cut would emit invalid UTF-8
// into the parent's context).
func TestTruncateSubagentReportRuneSafe(t *testing.T) {
	const suffix = "… (truncated)"
	cap := MaxSubagentReportBytes
	cases := []struct {
		name    string
		report  string
		wantCap bool // result is truncated (suffix appended)
	}{
		{"empty", "", false},
		{"short ascii", "hello", false},
		{"exactly at cap", strings.Repeat("a", cap), false},
		{"ascii over cap", strings.Repeat("a", cap+1), true},
		{"emoji straddles cut", strings.Repeat("a", cap-2) + "🦄", true},
		{"cjk straddles cut", strings.Repeat("a", cap-1) + "中", true},
		{"two-byte rune straddles cut", strings.Repeat("a", cap-1) + "é", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateSubagentReport(tc.report)
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8 (len=%d)", len(got))
			}
			if tc.wantCap {
				if !strings.HasSuffix(got, suffix) {
					t.Fatalf("expected %q suffix, got tail %q", suffix, got[len(got)-32:])
				}
				if len(got) > cap+len(suffix) {
					t.Fatalf("truncated result exceeds byte cap: %d > %d", len(got), cap+len(suffix))
				}
			} else if got != tc.report {
				t.Fatalf("short report was modified: %q", got)
			}
		})
	}
}
