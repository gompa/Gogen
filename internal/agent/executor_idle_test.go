package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExecuteCommandIdleTimeoutKillsSilentCommand pins the core guarantee:
// a foreground command that produces no output for the idle window is
// killed, with a diagnostic that names the idle condition (not a generic
// "cancelled"), and the kill lands well before the command's own runtime.
func TestExecuteCommandIdleTimeoutKillsSilentCommand(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	exec.SetIdleTimeout(300 * time.Millisecond)

	start := time.Now()
	_, err := exec.ExecuteCommand(context.Background(), "sleep 10")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an idle-timeout error for a silent command")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("error = %q, want an idle-timeout diagnostic", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("silent command took %s to be killed, want well under its 10s runtime", elapsed)
	}
}

// TestExecuteCommandIdleTimeoutAllowsActiveCommand is the mirror case: a
// command whose TOTAL runtime exceeds the idle window but that keeps
// producing output must run to completion — the window resets on every
// chunk, so there is no wall-clock cap.
func TestExecuteCommandIdleTimeoutAllowsActiveCommand(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	exec.SetIdleTimeout(500 * time.Millisecond)

	// Prints every ~100ms for ~1.6s: total runtime is 3x the idle window,
	// but no silence window ever approaches it.
	out, err := exec.ExecuteCommand(context.Background(),
		"for i in 1 2 3 4 5 6 7 8; do echo tick; sleep 0.1; done")
	if err != nil {
		t.Fatalf("active command must not be killed: %v", err)
	}
	if got := strings.Count(out, "tick"); got != 8 {
		t.Fatalf("output = %q, want 8 ticks", out)
	}
}

// TestExecuteCommandIdleTimeoutDefaultApplies pins the config semantic:
// when the setter is never called (config 0 = "use default"), the
// built-in 120s window applies — a 2s silent command must survive it.
func TestExecuteCommandIdleTimeoutDefaultApplies(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	// Never call SetIdleTimeout: the built-in default must apply. The
	// default is 120s, so a 2s silent command must SURVIVE (the default
	// window is not yet reached) — proving the default is the long one,
	// and a separately configured short window is what kills.
	out, err := exec.ExecuteCommand(context.Background(), "sleep 2; echo ok")
	if err != nil {
		t.Fatalf("2s silent command must survive the 120s default idle window: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("output = %q, want ok", out)
	}
}
