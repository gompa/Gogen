package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverInstructionsNearestFirst pins the walk order: files in the
// working directory win over files in parent directories, and AGENTS.md
// wins over CLAUDE.md within one directory.
func TestDiscoverInstructionsNearestFirst(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	child := filepath.Join(parent, "child")
	writeFile(t, filepath.Join(parent, "AGENTS.md"), "parent agents")
	writeFile(t, filepath.Join(parent, "CLAUDE.md"), "parent claude")
	writeFile(t, filepath.Join(child, "CLAUDE.md"), "child claude")

	files, err := DiscoverInstructions(child)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, filepath.Base(f.Path)+":"+f.Content)
	}
	want := []string{
		"CLAUDE.md:child claude",
		"AGENTS.md:parent agents",
		"CLAUDE.md:parent claude",
	}
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestDiscoverInstructionsStopsAtGitRoot pins the project-root boundary:
// a .git marker stops the walk (the marker directory's own files are
// included; ancestors are not).
func TestDiscoverInstructionsStopsAtGitRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git"), "gitdir: .")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root agents")
	sub := filepath.Join(root, "pkg")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "pkg agents")
	// Above the git root: must never be read.
	outer := filepath.Dir(root)
	writeFile(t, filepath.Join(outer, "AGENTS.md"), "outer agents")

	files, err := DiscoverInstructions(sub)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, filepath.Base(f.Path)+":"+f.Content)
	}
	want := []string{"AGENTS.md:pkg agents", "AGENTS.md:root agents"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

// TestDiscoverInstructionsStopsAtHome pins the home boundary: the home
// directory itself is never read, only project directories below it.
func TestDiscoverInstructionsStopsAtHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, "AGENTS.md"), "home agents")
	proj := filepath.Join(home, "proj")
	writeFile(t, filepath.Join(proj, "AGENTS.md"), "proj agents")

	files, err := DiscoverInstructions(proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Content != "proj agents" {
		t.Fatalf("files = %+v, want only the project file", files)
	}
}

// TestDiscoverInstructionsDedup collapses identical trimmed content to its
// first occurrence.
func TestDiscoverInstructionsDedup(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "  same rules\n")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "same rules")

	files, err := DiscoverInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %+v, want 1 (deduped)", files)
	}
}

// TestDiscoverInstructionsCaps pins the byte budgets: an oversized file is
// skipped, and collection stops before the total cap is exceeded.
func TestDiscoverInstructionsCaps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("a", MaxInstructionFileBytes+1))
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "small claude")

	files, err := DiscoverInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Content != "small claude" {
		t.Fatalf("oversized file must be skipped: %+v", files)
	}

	// Total cap: three per-file-legal files (25KB each) that would push the
	// aggregate past MaxInstructionTotalBytes; discovery must stop before
	// the third is added.
	root2 := t.TempDir()
	writeFile(t, filepath.Join(root2, "AGENTS.md"), strings.Repeat("b", 25*1024))
	writeFile(t, filepath.Join(root2, "CLAUDE.md"), strings.Repeat("c", 25*1024))
	sub := filepath.Join(root2, "sub")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), strings.Repeat("d", 25*1024))

	files, err = DiscoverInstructions(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2 (the third would exceed the total cap)", len(files))
	}
	total := 0
	for _, f := range files {
		total += len(f.Content)
	}
	if total > MaxInstructionTotalBytes {
		t.Fatalf("total %d exceeds cap %d", total, MaxInstructionTotalBytes)
	}
}

// TestDiscoverInstructionsMissingRoot returns no files, never an error.
func TestDiscoverInstructionsMissingRoot(t *testing.T) {
	files, err := DiscoverInstructions(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %+v, want none", files)
	}
}

// TestRenderInstructions pins the rendered form: per-file headers, trimmed
// content, and a trailing-trimmed joined body.
func TestRenderInstructions(t *testing.T) {
	got := RenderInstructions([]InstructionFile{
		{Path: "/a/AGENTS.md", Content: "one"},
		{Path: "/a/CLAUDE.md", Content: "two"},
	})
	want := "## From /a/AGENTS.md\n\none\n\n## From /a/CLAUDE.md\n\ntwo"
	if got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
	if RenderInstructions(nil) != "" {
		t.Fatal("empty input must render empty")
	}
}

// TestLoadInstructionsEmpty returns "" when no instruction files exist.
func TestLoadInstructionsEmpty(t *testing.T) {
	got, err := LoadInstructions(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("load = %q, want empty", got)
	}
}
