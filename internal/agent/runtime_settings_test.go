package agent

import (
	"testing"
	"time"
)

// TestExecutorRuntimeSetters verifies the live security/execution setters:
// each swap is visible to the runtime read accessors. The fields themselves
// are unexported — construction-time configuration (setup.go, tests) goes
// through the same setters, so no code path can bypass liveMu.
func TestExecutorRuntimeSetters(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	if !exec.DeleteApprovalRequired() {
		t.Fatal("default delete approval should be required")
	}
	if got := exec.commandGuard(); got == nil || got.Mode != "blocklist" {
		t.Fatalf("default guard = %+v, want blocklist", got)
	}
	if exec.sandbox() != "off" {
		t.Fatalf("default sandbox = %q, want off", exec.sandbox())
	}
	if exec.idleTimeout() != 0 {
		t.Fatalf("default idle timeout = %v, want 0 (meaning: the built-in default)", exec.idleTimeout())
	}

	exec.SetCommandGuard("allowlist", []string{"ls", "cat"})
	g := exec.commandGuard()
	if g == nil || g.Mode != "allowlist" || len(g.Allowlist) != 2 {
		t.Fatalf("guard after SetCommandGuard = %+v, want allowlist [ls cat]", g)
	}

	exec.SetDeleteApproval(false)
	if exec.DeleteApprovalRequired() {
		t.Fatal("delete approval should be off after SetDeleteApproval(false)")
	}
	if exec.deleteApprovalRequired(t.Context()) {
		t.Fatal("deleteApprovalRequired(ctx) should be false after the toggle")
	}
	exec.SetDeleteApproval(true)
	if !exec.deleteApprovalRequired(t.Context()) {
		t.Fatal("deleteApprovalRequired(ctx) should be true again")
	}

	exec.SetSandbox("bwrap")
	if exec.sandbox() != "bwrap" {
		t.Fatalf("sandbox = %q, want bwrap", exec.sandbox())
	}

	exec.SetIdleTimeout(45 * time.Second)
	if exec.idleTimeout() != 45*time.Second {
		t.Fatalf("idle timeout = %v, want 45s", exec.idleTimeout())
	}
}
