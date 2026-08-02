package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestExecuteCommandCancelStopsChildren(t *testing.T) {
	dir := t.TempDir()
	ex := NewExecutor(dir)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		// echo keeps sh alive as parent of sleep; without process-group kill
		// CombinedOutput hangs after cancel with children holding pipes.
		out, err = ex.ExecuteCommand(ctx, "echo partial && sleep 30")
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteCommand hung after cancel")
	}
	if err == nil {
		t.Fatal("expected cancel error")
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("expected partial output before cancel, got %q", out)
	}
}

func TestExecuteCommandCancelPipeline(t *testing.T) {
	dir := t.TempDir()
	ex := NewExecutor(dir)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = ex.ExecuteCommand(ctx, "sleep 30 | cat")
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline ExecuteCommand hung after cancel")
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

func TestExecuteCommandStreamsOutputToSink(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	var mu sync.Mutex
	var gotCmd string
	var chunks []string
	sink := func(command, chunk string) {
		mu.Lock()
		defer mu.Unlock()
		if gotCmd == "" {
			gotCmd = command
		}
		chunks = append(chunks, chunk)
	}

	out, err := exec.ExecuteCommand(
		ContextWithToolOutput(context.Background(), sink),
		"printf 'one\\ntwo\\n' && echo err >&2",
	)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	joined := strings.Join(chunks, "")
	cmd := gotCmd
	mu.Unlock()

	if cmd != "printf 'one\\ntwo\\n' && echo err >&2" {
		t.Fatalf("sink received wrong command: %q", cmd)
	}
	// Chunk boundaries are pipe-dependent, but the concatenation must be
	// byte-identical to the returned (accumulated) output, and both stdout
	// and stderr must be present.
	if joined != out {
		t.Fatalf("sink output %q != returned output %q", joined, out)
	}
	if !strings.Contains(joined, "one\ntwo") || !strings.Contains(joined, "err") {
		t.Fatalf("sink output missing stdout/stderr content: %q", joined)
	}
}

func TestExecuteCommandSinkReceivesPartialOutputOnTimeout(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	var mu sync.Mutex
	joined := ""
	sink := func(_ string, chunk string) {
		mu.Lock()
		joined += chunk
		mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(
		ContextWithToolOutput(context.Background(), sink),
		50*time.Millisecond,
	)
	defer cancel()

	out, err := exec.ExecuteCommand(ctx, "echo partial && sleep 2")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	sinkOut := joined
	mu.Unlock()
	if !strings.Contains(out, "partial") || !strings.Contains(sinkOut, "partial") {
		t.Fatalf("expected partial output in both return and sink; out=%q sink=%q", out, sinkOut)
	}
}

func TestExecuteCommandNoSinkMatchesReturnedOutput(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "printf 'x\\ny\\n' && echo z >&2")
	if err != nil {
		t.Fatal(err)
	}
	if out != "x\ny\nz\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}
