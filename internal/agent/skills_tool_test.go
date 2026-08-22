package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/skills"
)

func newTestSkillAgent(t *testing.T, enabled bool) *Agent {
	t.Helper()
	prov := &llm.MockProvider{}
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	if enabled {
		proj := t.TempDir()
		if err := os.MkdirAll(filepath.Join(proj, ".gogen", "skills", "review"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proj, ".gogen", "skills", "review", "SKILL.md"), []byte("---\ndescription: Review checklist\n---\nChecklist"), 0o644); err != nil {
			t.Fatal(err)
		}
		a.SetSkillsManager(skills.NewManager(proj, false))
		a.SetSkillsEnabled(true)
	}
	return a
}

// TestSkillToolGating pins the MCP-style gating: with skills off the tool
// appears nowhere (llmTools, AllowedToolNames, executeTool); with skills on
// it is registered and executable.
func TestSkillToolGating(t *testing.T) {
	a := newTestSkillAgent(t, false)
	for _, td := range a.llmTools() {
		if td.Name == "skill" {
			t.Fatal("skill tool must not be registered when skills is off")
		}
	}
	if _, ok := a.AllowedToolNames()["skill"]; ok {
		t.Fatal("skill must not be allowed when skills is off")
	}
	if _, err := a.executeToolCall(t, "skill", map[string]any{"action": "list"}); err == nil {
		t.Fatal("executeTool must reject skill when skills is off")
	}

	a = newTestSkillAgent(t, true)
	found := false
	for _, td := range a.llmTools() {
		if td.Name == "skill" {
			found = true
		}
	}
	if !found {
		t.Fatal("skill tool must be registered when skills is on")
	}
	if _, ok := a.AllowedToolNames()["skill"]; !ok {
		t.Fatal("skill must be allowed when skills is on")
	}
	out, err := a.executeToolCall(t, "skill", map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("skill list: %v", err)
	}
	if !strings.Contains(out, "review") || !strings.Contains(out, "Review checklist") {
		t.Fatalf("skill list = %q", out)
	}
	out, err = a.executeToolCall(t, "skill", map[string]any{"action": "read", "name": "review"})
	if err != nil {
		t.Fatalf("skill read: %v", err)
	}
	if !strings.Contains(out, "Checklist") {
		t.Fatalf("skill read = %q", out)
	}
	if _, err := a.executeToolCall(t, "skill", map[string]any{"action": "read", "name": "missing"}); err == nil {
		t.Fatal("missing skill must error")
	}
}

// TestSkillToolPlanMode pins the plan-mode exception: skill list/read are
// read-only, so the tool stays allowed in plan mode (like the board).
func TestSkillToolPlanMode(t *testing.T) {
	a := newTestSkillAgent(t, true)
	a.SetMode(ModePlan)
	if _, ok := a.AllowedToolNames()["skill"]; !ok {
		t.Fatal("skill must be allowed in plan mode (read-only)")
	}
	out, err := a.executeToolCall(t, "skill", map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("skill list in plan mode: %v", err)
	}
	if !strings.Contains(out, "review") {
		t.Fatalf("skill list = %q", out)
	}
}

// executeToolCall routes one tool call through the agent's executeTool.
func (a *Agent) executeToolCall(t *testing.T, name string, args map[string]any) (string, error) {
	t.Helper()
	tc := llm.ToolCall{ID: "c1", Name: name, Args: args}
	return a.executeTool(t.Context(), tc)
}
