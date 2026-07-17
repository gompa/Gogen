package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecuteCommandPreservesOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "echo hello && exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected command output in result, got %q", out)
	}
}

func TestExecuteCommandPreservesOutputOnTimeout(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, err := exec.ExecuteCommand(ctx, "echo partial && sleep 2")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("expected partial output before timeout, got %q", out)
	}
}

func TestExecuteCommandSuccess(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "echo ok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestExecuteCommandUsesWorkingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("found"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "cat marker.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "found" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestExecuteCommandBwrapMissingErrors(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.Sandbox = "bwrap"
	// Force LookPath failure by using a PATH without bwrap.
	t.Setenv("PATH", dir)

	_, err := exec.ExecuteCommand(context.Background(), "echo hi")
	if err == nil {
		t.Fatal("expected error when bwrap is missing")
	}
	if !strings.Contains(err.Error(), "bwrap not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteCommandUnknownSandboxErrors(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.Sandbox = "docker"

	_, err := exec.ExecuteCommand(context.Background(), "echo hi")
	if err == nil {
		t.Fatal("expected error for unknown sandbox")
	}
	if !strings.Contains(err.Error(), "unknown command_sandbox") {
		t.Fatalf("unexpected error: %v", err)
	}
}
