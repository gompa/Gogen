package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoOverview(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "agent", "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "agent", "b.go"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "main.go"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.RepoOverview()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "internal/") || !strings.Contains(out, "2 files") {
		t.Fatalf("expected internal/ with 2 files: %q", out)
	}
	if !strings.Contains(out, "cmd/") {
		t.Fatalf("expected cmd/: %q", out)
	}
	if !strings.Contains(out, "go.mod") || !strings.Contains(out, "Suggested reads") {
		t.Fatalf("expected root files and hints: %q", out)
	}
}

func TestRepoOverviewSkipsGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "objects", "x"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.RepoOverview()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".git/") {
		t.Fatalf(".git should be skipped: %q", out)
	}
	if !strings.Contains(out, "1 files") {
		t.Fatalf("expected 1 file total: %q", out)
	}
}
