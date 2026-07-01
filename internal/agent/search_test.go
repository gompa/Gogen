package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchCodeGoFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n\nfunc hello() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("not in go files\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "func hello", "", "*.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello.go") || !strings.Contains(out, "func hello") {
		t.Fatalf("unexpected output: %q", out)
	}
	if _, err := exec.LookPath("rg"); err != nil {
		if !strings.Contains(out, "go fallback") {
			t.Fatalf("expected go fallback note: %q", out)
		}
	}
}

func TestSearchCodeNoMatches(t *testing.T) {
	dir := t.TempDir()
	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "missing-pattern-xyz", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSearchCodeSubpath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package pkg\n\nvar Target = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "Target", "pkg", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/a.go:3:var Target = 1") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSearchCodeUsesRipgrepWhenAvailable(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "findme.go"), []byte("const Needle = \"unique-needle-42\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "unique-needle-42", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "findme.go") || !strings.Contains(out, "unique-needle-42") {
		t.Fatalf("unexpected output: %q", out)
	}
	if strings.Contains(out, "go fallback") {
		t.Fatalf("expected rg path, got go fallback: %q", out)
	}
}

func TestPrefixRelPaths(t *testing.T) {
	got := prefixRelPaths("a.go:1:line\nb.go:2:other", "internal/agent")
	want := "internal/agent/a.go:1:line\ninternal/agent/b.go:2:other"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPrefixRelPathsPreservesEmptyLines(t *testing.T) {
	got := prefixRelPaths("a.go:1:line\n\nb.go:2:other", "pkg")
	want := "pkg/a.go:1:line\n\npkg/b.go:2:other"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSearchCodeDotDirRequiresSubpath(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, "ci.yml"), []byte("name: unique-workflow-marker-99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("unique-workflow-marker-99=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)

	out, err := executor.SearchCode(context.Background(), "unique-workflow-marker-99", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Fatalf("root search should skip hidden paths, got %q", out)
	}

	out, err = executor.SearchCode(context.Background(), "unique-workflow-marker-99", ".github", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".github/workflows/ci.yml") {
		t.Fatalf("expected subpath search in .github, got %q", out)
	}
}

func TestSearchCodeGlobPathPatternGoFallback(t *testing.T) {
	dir := t.TempDir()
	internal := filepath.Join(dir, "internal")
	if err := os.MkdirAll(internal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("var RootNeedle = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internal, "target.go"), []byte("var TargetNeedle = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "TargetNeedle", "", "internal/*.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "internal/target.go") {
		t.Fatalf("expected path-scoped glob match, got %q", out)
	}
	if strings.Contains(out, "root.go") {
		t.Fatalf("root.go should not match internal/*.go, got %q", out)
	}
}

func TestSearchCodeContextLinesGoFallback(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\nneedle here\nline4\nline5\n"
	if err := os.WriteFile(filepath.Join(dir, "ctx.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "needle", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line2") || !strings.Contains(out, "needle here") || !strings.Contains(out, "line4") {
		t.Fatalf("expected context lines around match, got %q", out)
	}
}

func TestSearchCodeContextLinesRipgrep(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}

	dir := t.TempDir()
	content := "alpha\nbeta\nneedle here\ndelta\nepsilon\n"
	if err := os.WriteFile(filepath.Join(dir, "ctx.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "needle", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "beta") || !strings.Contains(out, "needle here") || !strings.Contains(out, "delta") {
		t.Fatalf("expected rg context output, got %q", out)
	}
}
