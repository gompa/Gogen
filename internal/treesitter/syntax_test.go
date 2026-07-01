//go:build cgo

package treesitter_test

import (
	"strings"
	"testing"

	"gogen/internal/treesitter"
)

func TestCheckGoSyntaxError(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")
	src := []byte("package main\n\nfunc main() {\n")
	issues := treesitter.Check("main.go", src)
	if len(issues) == 0 {
		t.Fatal("expected syntax issues for broken Go source")
	}
}

func TestCheckGoValid(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")
	src := []byte("package main\n\nfunc main() {}\n")
	issues := treesitter.Check("main.go", src)
	if len(issues) != 0 {
		t.Fatalf("expected no issues, got %v", issues)
	}
}

func TestCheckUnsupportedExtension(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")
	issues := treesitter.Check("readme.txt", []byte("not code"))
	if len(issues) != 0 {
		t.Fatalf("expected no issues for unsupported ext, got %v", issues)
	}
}

func TestDisabled(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "off")
	src := []byte("package main\n\nfunc main() {\n")
	if note := treesitter.FormatCheck("main.go", src); note != "" {
		t.Fatalf("expected empty note when disabled, got %q", note)
	}
}

func TestLangFilter(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")
	t.Setenv("GOGEN_TREESITTER_LANGS", "json")
	langs := treesitter.BundledLanguages()
	if len(langs) != 1 || langs[0] != "json" {
		t.Fatalf("expected only json, got %v", langs)
	}
	brokenJSON := []byte("{")
	if len(treesitter.Check("x.json", brokenJSON)) == 0 {
		t.Fatal("expected json syntax issue")
	}
	if len(treesitter.Check("x.go", []byte("package main\n\nfunc main() {\n"))) != 0 {
		t.Fatal("go should be filtered out")
	}
}

func TestFormatCheckIncludesPath(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "on")
	note := treesitter.FormatCheck("pkg/main.go", []byte("package main\n\nfunc main() {\n"))
	if !strings.Contains(note, "main.go") {
		t.Fatalf("expected path in note, got %q", note)
	}
}
