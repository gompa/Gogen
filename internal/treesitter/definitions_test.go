//go:build cgo

package treesitter_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/treesitter"
)

func TestListDefinitionsGo(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	t.Setenv("GOGEN_TREESITTER", "on")

	src, err := os.ReadFile(filepath.Join("..", "agent", "definitions.go"))
	if err != nil {
		t.Fatal(err)
	}
	defs, err := treesitter.ListDefinitions("definitions.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("expected definitions")
	}
	found := false
	for _, d := range defs {
		if d.Name == "ListDefinitions" && (d.Kind == "function" || d.Kind == "method") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ListDefinitions function in defs: %+v", defs)
	}
}

func TestListDefinitionsUnsupported(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")
	_, err := treesitter.ListDefinitions("readme.txt", []byte("hello"))
	if err != treesitter.ErrUnsupported {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestFormatDefinitions(t *testing.T) {
	out := treesitter.FormatDefinitions("a.go", []treesitter.Definition{
		{Line: 10, Kind: "function", Name: "Foo"},
		{Line: 2, Kind: "type", Name: "Bar"},
	})
	if !strings.Contains(out, "L2") || !strings.Contains(out, "Bar") {
		t.Fatalf("unexpected format: %q", out)
	}
}
