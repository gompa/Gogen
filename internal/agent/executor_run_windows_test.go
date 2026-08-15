//go:build windows

package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These tests exercise the embedded POSIX interpreter (executor_run_windows.go)
// with commands that need no external shell and only stock Windows binaries
// (cmd.exe) or interpreter builtins, so they pass without git-bash/msys.

// TestWindowsShellBuiltins verifies the interpreter's builtins cover the
// shell constructs the agent's commands rely on.
func TestWindowsShellBuiltins(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	out, err := exec.ExecuteCommand(context.Background(), "echo hello && printf 'world\n'")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("output = %q, want hello + world", out)
	}
}

// TestWindowsShellExternalCommand verifies external binaries are resolved
// via PATH and exec'd natively (cmd.exe ships with every Windows install).
func TestWindowsShellExternalCommand(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	out, err := exec.ExecuteCommand(context.Background(), "cmd /c echo external-ok")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "external-ok") {
		t.Fatalf("output = %q, want external-ok", out)
	}
}

// TestWindowsShellExitStatus verifies non-zero exits surface as
// "execution error: exit status N", matching the Unix sh path.
func TestWindowsShellExitStatus(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	_, err := exec.ExecuteCommand(context.Background(), "exit 3")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want exit status 3", err)
	}
}

// TestWindowsShellCommandNotFound verifies a missing binary reports the
// shell's 127 exit status (like sh).
func TestWindowsShellCommandNotFound(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	_, err := exec.ExecuteCommand(context.Background(), "definitely-not-a-real-command-xyz")
	if err == nil || !strings.Contains(err.Error(), "exit status 127") {
		t.Fatalf("err = %v, want exit status 127", err)
	}
}

// TestWindowsShellTimeoutKillsTree verifies the timeout path terminates the
// command promptly instead of waiting it out, and that the kill reaches the
// process tree (taskkill /T).
func TestWindowsShellTimeoutKillsTree(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	// cmd /c ping -n 30 would otherwise run ~30s.
	_, err := exec.ExecuteCommand(ctx, "cmd /c ping -n 30 127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout kill took %s, want prompt tree kill", elapsed)
	}
}

// TestWindowsBackgroundJobInterp verifies background jobs run through the
// embedded interpreter and stdin input works end-to-end (in-memory pipe).
func TestWindowsBackgroundJobInterp(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("while read line; do echo \"got: $line\"; done")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := a.BackgroundJobInput(id, "hello", true); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(s, "got: hello") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("echoed line never appeared in job output")
}
