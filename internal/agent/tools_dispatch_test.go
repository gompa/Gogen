package agent

import (
	"context"
	"strings"
	"testing"
)

// TestShowDiffStagedRejectsNonBool verifies that a non-boolean "staged" arg
// surfaces an error rather than being silently coerced to false.
func TestShowDiffStagedRejectsNonBool(t *testing.T) {
	a := &Agent{Mode: ModeAct, Executor: &Executor{WorkingDir: t.TempDir()}}

	_, err := a.executeTool(context.Background(), llmToolCall("show_diff", map[string]interface{}{
		"staged": "true", // string instead of bool
	}))
	if err == nil {
		t.Fatal("expected error for non-bool staged arg")
	}
	if !strings.Contains(err.Error(), "staged") {
		t.Fatalf("expected error to mention staged, got: %v", err)
	}
}

// TestShowDiffStagedBoolAccepted verifies that a proper boolean staged arg is
// accepted (and does not error on arg parsing). The call may still fail later
// if git is unavailable, so we only assert no *parsing* error.
func TestShowDiffStagedBoolAccepted(t *testing.T) {
	a := &Agent{Mode: ModeAct, Executor: &Executor{WorkingDir: t.TempDir()}}

	_, err := a.executeTool(context.Background(), llmToolCall("show_diff", map[string]interface{}{
		"staged": true,
	}))
	// We expect either success or a git-level error — but never an arg-type
	// error mentioning "staged".
	if err != nil && strings.Contains(err.Error(), "must be a boolean") {
		t.Fatalf("bool staged should be accepted, got: %v", err)
	}
}

// TestToolsSync ensures every built-in tool definition has a matching handler and vice versa.
func TestToolsSync(t *testing.T) {
	if err := ValidateToolSync(); err != nil {
		t.Fatal(err)
	}
}
