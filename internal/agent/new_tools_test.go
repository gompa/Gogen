package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGitStatusNotARepo(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	_, err := exec.GitStatus(context.Background(), "")
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
}

func TestGitStatusCleanRepo(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	exec := NewExecutor(dir)
	out, err := exec.GitStatus(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Working tree clean (no changes)" {
		t.Fatalf("got %q, want clean message", out)
	}
}

func TestGitStatusWithChanges(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	out, err := exec.GitStatus(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "Working tree clean (no changes)" {
		t.Fatal("expected changes, got clean message")
	}
}

func TestGitStatusScopedPath(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	out, err := exec.GitStatus(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "Working tree clean (no changes)" {
		t.Fatal("expected changes for a.txt")
	}
}

// gitInit initializes a git repo in dir with a minimal initial commit.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, cmd := range []string{
		"git init",
		"git config user.email test@test.com",
		"git config user.name test",
	} {
		if err := execShell(dir, cmd); err != nil {
			t.Fatalf("git setup %q: %v", cmd, err)
		}
	}
	// Initial commit so the tree has a HEAD to compare against.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := execShell(dir, "git add .gitignore"); err != nil {
		t.Fatal(err)
	}
	if err := execShell(dir, "git commit -m init"); err != nil {
		t.Fatal(err)
	}
}

func execShell(dir, command string) error {
	exec := NewExecutor(dir)
	_, err := exec.ExecuteCommand(context.Background(), command)
	return err
}
