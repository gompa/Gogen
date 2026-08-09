package agent_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/agent"
)

func TestExecutorListDefinitions(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	t.Setenv("GOGEN_TREESITTER", "on")

	dir := t.TempDir()
	src := []byte("package sample\n\nfunc Hello() {}\n\ntype Widget struct{}\n")
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	exec := agent.NewExecutor(dir)
	out, err := exec.ListDefinitions("sample.go")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected output")
	}
}

// TestExecutorListDefinitionsLargeFile verifies that definitions are parsed
// from raw file content even when the file exceeds readFileWarnBytes, which
// would previously cause ReadFile to prepend a "Warning: file is ..." header
// and corrupt the tree-sitter parse.
func TestExecutorListDefinitionsLargeFile(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	t.Setenv("GOGEN_TREESITTER", "on")

	dir := t.TempDir()
	// Build a Go file larger than readFileWarnBytes (100 KiB) with a real
	// function definition near the end, padded by a long comment.
	pad := make([]byte, 0)
	for i := 0; i < 3000; i++ {
		pad = append(pad, []byte(fmt.Sprintf("// padding line %d\n", i))...)
	}
	src := append([]byte("package sample\n\n"), pad...)
	src = append(src, []byte("func RealFunc() {}\n")...)
	if err := os.WriteFile(filepath.Join(dir, "big.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	exec := agent.NewExecutor(dir)
	out, err := exec.ListDefinitions("big.go")
	if err != nil {
		t.Fatalf("ListDefinitions failed: %v", err)
	}
	if out == "" {
		t.Fatal("expected definitions for large file")
	}
	if !strings.Contains(out, "RealFunc") {
		t.Fatalf("expected RealFunc in definitions, got:\n%s", out)
	}
}

// TestExecutorListDefinitionsTextFallback verifies that list_definitions
// degrades to a line-based text outline when tree-sitter is disabled, instead
// of erroring. It must work in every build (cgo or not) and must not echo
// comment lines.
func TestExecutorListDefinitionsTextFallback(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "off")

	dir := t.TempDir()
	src := `package sample

func Hello() {}

// a comment, not a definition
type Widget struct{}

func (s *Server) Handle() {}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := agent.NewExecutor(dir)
	out, err := exec.ListDefinitions("sample.go")
	if err != nil {
		t.Fatalf("text fallback should not error: %v", err)
	}
	if !strings.Contains(out, "text outline") {
		t.Fatalf("expected text-outline marker, got:\n%s", out)
	}
	for _, want := range []string{"Hello", "Widget", "Handle"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in outline, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "comment") {
		t.Fatalf("comment line leaked into outline:\n%s", out)
	}
}
