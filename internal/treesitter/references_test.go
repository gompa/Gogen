//go:build cgo

package treesitter_test

import (
	"os"
	"testing"

	"gogen/internal/treesitter"
)

func TestFindSymbolReferencesGo(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	t.Setenv("GOGEN_TREESITTER", "on")

	src := []byte(`package main

var x int

func main() {
	x = 1
}
`)
	refs, err := treesitter.FindSymbolReferences("test.go", src, "x")
	if err != nil {
		t.Fatalf("FindSymbolReferences: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("expected references to 'x', got none")
	}
}

func TestFindSymbolReferencesNotFound(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")

	src := []byte(`package main

func main() {}
`)
	refs, err := treesitter.FindSymbolReferences("test.go", src, "nonexistent")
	if err != nil {
		t.Fatalf("FindSymbolReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no references, got %d", len(refs))
	}
}

func TestFindSymbolReferencesEmptySymbol(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")

	_, err := treesitter.FindSymbolReferences("test.go", []byte("package main"), "")
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
}

func TestFindSymbolReferencesDisabled(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "off")

	_, err := treesitter.FindSymbolReferences("test.go", []byte("package main"), "x")
	if err != treesitter.ErrDisabled {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

func TestFindSymbolReferencesUnsupported(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")

	_, err := treesitter.FindSymbolReferences("readme.txt", []byte("hello"), "x")
	if err != treesitter.ErrUnsupported {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestReferenceSearchSupported(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")

	if !treesitter.ReferenceSearchSupported("main.go") {
		t.Error("expected Go to be supported")
	}
	if treesitter.ReferenceSearchSupported("readme.txt") {
		t.Error("expected .txt to be unsupported")
	}
}

func TestReferenceSearchSupportedDisabled(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "off")

	if treesitter.ReferenceSearchSupported("main.go") {
		t.Error("expected disabled to return false")
	}
}

func TestFormatReferenceMatches(t *testing.T) {
	refs := []treesitter.Reference{
		{Line: 1, Text: "var x int"},
		{Line: 5, Text: "x = 1"},
	}
	lines := treesitter.FormatReferenceMatches("foo.go", refs)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0] != "foo.go:1:var x int" {
		t.Errorf("unexpected line: %q", lines[0])
	}
}
