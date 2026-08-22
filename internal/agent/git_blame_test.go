package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGitCmd runs git with args in dir, failing the test on error.
func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes content to name in dir and commits it, returning the
// resulting commit hash.
func commitFile(t *testing.T, dir, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, dir, "add", name)
	runGitCmd(t, dir, "commit", "-m", message)
	return runGitCmd(t, dir, "rev-parse", "HEAD")
}

func TestGitBlameCommittedFile(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitFile(t, dir, "tracked.go", "package main\n\nvar ErrorCode = 1\n", "add tracked.go")

	exec := NewExecutor(dir)
	out, err := exec.GitBlame(context.Background(), "tracked.go", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "package main") || !strings.Contains(out, "var ErrorCode = 1") {
		t.Fatalf("blame output missing file content: %q", out)
	}
	if !strings.Contains(out, "(test ") {
		t.Fatalf("blame output missing author: %q", out)
	}
}

func TestGitBlameLineRange(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitFile(t, dir, "tracked.go", "one\ntwo\nthree\nfour\nfive\n", "add tracked.go")

	exec := NewExecutor(dir)
	out, err := exec.GitBlame(context.Background(), "tracked.go", "", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			got++
		}
		if strings.Contains(line, "one") || strings.Contains(line, "four") || strings.Contains(line, "five") {
			t.Fatalf("line outside -L range present: %q", out)
		}
	}
	if got != 2 {
		t.Fatalf("expected exactly 2 blamed lines, got %d: %q", got, out)
	}
}

func TestGitBlameAtRef(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	first := commitFile(t, dir, "tracked.go", "original line\n", "add tracked.go")
	commitFile(t, dir, "tracked.go", "replaced line\n", "replace tracked.go")

	exec := NewExecutor(dir)

	out, err := exec.GitBlame(context.Background(), "tracked.go", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced line") {
		t.Fatalf("working-tree blame should show current content: %q", out)
	}

	out, err = exec.GitBlame(context.Background(), "tracked.go", first, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "original line") || strings.Contains(out, "replaced line") {
		t.Fatalf("ref blame should show historical content: %q", out)
	}
}

func TestGitBlameEmptyFile(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitFile(t, dir, "empty.txt", "", "add empty file")

	exec := NewExecutor(dir)
	out, err := exec.GitBlame(context.Background(), "empty.txt", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No blame information") {
		t.Fatalf("unexpected output for empty file: %q", out)
	}
}

func TestGitBlameValidation(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitFile(t, dir, "tracked.go", "a\nb\nc\n", "add tracked.go")
	exec := NewExecutor(dir)

	cases := []struct {
		name      string
		file      string
		ref       string
		lineStart int
		lineEnd   int
		wantErr   string
	}{
		{"missing file", "", "", 0, 0, "file is required"},
		{"path escapes workspace", "../outside.txt", "", 0, 0, ""},
		{"ref looks like a flag", "tracked.go", "--all", 0, 0, "must not start with '-'"},
		{"ref with bad chars", "tracked.go", "HEAD;rm", 0, 0, "invalid git ref"},
		{"start without end", "tracked.go", "", 2, 0, "given together"},
		{"end without start", "tracked.go", "", 0, 2, "given together"},
		{"negative line", "tracked.go", "", -1, 3, "must be positive"},
		{"start after end", "tracked.go", "", 3, 2, "must not exceed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec.GitBlame(context.Background(), tc.file, tc.ref, tc.lineStart, tc.lineEnd)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestGitBlameNotARepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	if _, err := exec.GitBlame(context.Background(), "plain.txt", "", 0, 0); err == nil {
		t.Fatal("expected error outside a git repo")
	}
}

func TestHandleGitBlameRequiresFileArg(t *testing.T) {
	a := &Agent{Executor: &Executor{WorkingDir: t.TempDir()}}
	if _, err := handleGitBlame(context.Background(), a, map[string]any{}); err == nil {
		t.Fatal("expected missing-argument error")
	} else if !strings.Contains(err.Error(), "file") {
		t.Fatalf("unexpected error: %v", err)
	}
}
