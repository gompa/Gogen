package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListFilesRecursive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sub", "b.go"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ListFiles(".", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/a.go") || !strings.Contains(out, "pkg/sub/b.go") {
		t.Fatalf("unexpected listing: %q", out)
	}
}

func TestGlobFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.GlobFiles("*.go", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") || strings.Contains(out, "readme.txt") {
		t.Fatalf("unexpected glob: %q", out)
	}
}

func TestReadFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFiles([]string{"a.txt", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "=== a.txt ===") || !strings.Contains(out, "alpha") {
		t.Fatalf("missing a.txt content: %q", out)
	}
	if !strings.Contains(out, "=== b.txt ===") || !strings.Contains(out, "beta") {
		t.Fatalf("missing b.txt content: %q", out)
	}
}
