package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/agent"
)

func TestAppendSyntaxCheckGoFile(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("tree-sitter syntax checks require CGO")
	}
	t.Setenv("GOGEN_TREESITTER", "on")

	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	path := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := exec.AppendSyntaxCheck("ok", "broken.go")
	if out == "ok" {
		t.Fatal("expected syntax note appended")
	}
}
