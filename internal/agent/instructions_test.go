package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEffectiveGuidelinesRefreshesOnDirChange pins the agent_instructions
// lifecycle: the rendered AGENTS.md/CLAUDE.md section follows the agent's
// CURRENT working dir — a /dir change re-renders it, and a stale project's
// instructions never linger in the system prompt.
func TestEffectiveGuidelinesRefreshesOnDirChange(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeInstructionsFile(t, filepath.Join(dirA, "AGENTS.md"), "rules from A")
	writeInstructionsFile(t, filepath.Join(dirB, "AGENTS.md"), "rules from B")

	exec := NewExecutor(dirA)
	a := NewAgent(nil, exec, nil)
	defer a.Close()
	a.SetProjectContext("", "project guidelines", "", "")
	a.SetInstructionsEnabled(true)
	a.RefreshWorkspaceInstructions(dirA)

	if got := a.EffectiveGuidelines(); !strings.Contains(got, "project guidelines") || !strings.Contains(got, "rules from A") {
		t.Fatalf("guidelines = %q, want project body + A's instructions", got)
	}
	// Disabled: the instruction section is never rendered (stale content
	// from a previous refresh is ignored).
	a.SetInstructionsEnabled(false)
	if got := a.EffectiveGuidelines(); strings.Contains(got, "rules from A") {
		t.Fatalf("disabled must not render instructions: %q", got)
	}
	// Back on + working-dir change: the section re-renders from the new
	// dir (SetWorkingDir refreshes it) and the old project's content is
	// gone.
	a.SetInstructionsEnabled(true)
	a.SetWorkingDir(dirB)
	if got := a.EffectiveGuidelines(); strings.Contains(got, "rules from A") || !strings.Contains(got, "rules from B") {
		t.Fatalf("guidelines after dir change = %q, want B's instructions only", got)
	}
}

// TestRefreshWorkspaceInstructionsNoopWhenDisabled pins the disabled
// fast-path: with the feature off, refresh performs no dir reads and the
// rendered guidelines stay the plain project body.
func TestRefreshWorkspaceInstructionsNoopWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	writeInstructionsFile(t, filepath.Join(dir, "AGENTS.md"), "never read")
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	defer a.Close()
	a.SetProjectContext("", "project guidelines", "", "")
	a.RefreshWorkspaceInstructions(dir)
	if got := a.EffectiveGuidelines(); got != "project guidelines" {
		t.Fatalf("guidelines = %q, want the plain project body", got)
	}
}

func writeInstructionsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
