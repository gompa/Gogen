package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunLintNoCommand(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	_, err := exec.RunLint(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error when no lint command detected")
	}
}

func TestRunLintWithOverride(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	out, err := exec.RunLint(context.Background(), "", "echo lint-clean")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}
}

func TestRunLintExtraArgs(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	out, err := exec.RunLint(context.Background(), "--fix", "echo lint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}
}

func TestDetectLintCommandGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := DetectLintCommand(dir, "")
	if got != "go vet ./..." {
		t.Fatalf("got %q, want %q", got, "go vet ./...")
	}
}

func TestDetectLintCommandOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := DetectLintCommand(dir, "ruff check .")
	if got != "ruff check ." {
		t.Fatalf("got %q, want %q", got, "ruff check .")
	}
}

func TestDetectLintCommandNoneDetected(t *testing.T) {
	dir := t.TempDir()
	got := DetectLintCommand(dir, "")
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

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

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	out, err := exec.MoveFile("src.txt", "dst.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source file should no longer exist")
	}
	got, err := os.ReadFile(filepath.Join(dir, "dst.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "content" {
		t.Fatalf("got %q, want %q", got, "content")
	}
}

func TestMoveFileToSubdirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	_, err := exec.MoveFile("file.txt", "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sub", "dir", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Fatalf("got %q, want %q", got, "data")
	}
}

func TestMoveFileSourceMissing(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	_, err := exec.MoveFile("nonexistent.txt", "dst.txt")
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestMoveFileSourceIsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "somedir"), 0o755); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	_, err := exec.MoveFile("somedir", "other")
	if err == nil {
		t.Fatal("expected error when source is a directory")
	}
}

func TestMoveFileOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	_, err := exec.MoveFile("src.txt", "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for destination outside working directory")
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
