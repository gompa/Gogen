package agent

import (
	"testing"
)

func TestParseDiffLineCountRejectsNegative(t *testing.T) {
	for _, part := range []string{"-1", "-1,2"} {
		if _, err := parseDiffLineCount(part); err == nil {
			t.Fatalf("expected error for %q", part)
		}
	}
}

func TestParseDiffLineCountAllowsZeroForNewFiles(t *testing.T) {
	got, err := parseDiffLineCount("0,0")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}

func TestParseDiffLineCountAcceptsPositive(t *testing.T) {
	got, err := parseDiffLineCount("5,3")
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("got %d want 5", got)
	}
}

func TestParseUnifiedDiffKeepsBlankContextLines(t *testing.T) {
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		"\n" + // bare blank line (LLM style) should become empty context
		"+// comment\n" +
		" func main() {\n" +
		" }\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 4 {
		t.Fatalf("oldLines=%d want 4 (blank context kept): %#v", len(h.oldLines), h.oldLines)
	}
	if h.oldLines[1] != "" {
		t.Fatalf("expected empty context line, got %q", h.oldLines[1])
	}
	if len(h.newLines) != 5 {
		t.Fatalf("newLines=%d want 5: %#v", len(h.newLines), h.newLines)
	}
}

func TestParseUnifiedDiffAcceptsCompactHunkHeader(t *testing.T) {
	diff := "--- a/main.go\n+++ b/main.go\n@@-1,2 +1,3@@\n package main\n+// x\n func main() {\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%v", len(files), files)
	}
	if files[0].hunks[0].oldStart != 1 {
		t.Fatalf("oldStart=%d", files[0].hunks[0].oldStart)
	}
}

func TestParseUnifiedDiffGitStyleMultiFile(t *testing.T) {
	diff := "" +
		"diff --git a/a.txt b/a.txt\n" +
		"index 111..222 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n" +
		"diff --git a/b.txt b/b.txt\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d want 2: %+v", len(files), files)
	}
	if normalizePatchPath(files[0].newName) != "a.txt" || normalizePatchPath(files[1].newName) != "b.txt" {
		t.Fatalf("paths=%q %q", files[0].newName, files[1].newName)
	}
	if len(files[0].hunks) != 1 || len(files[0].hunks[0].oldLines) != 1 {
		t.Fatalf("first hunk corrupted: %#v", files[0].hunks)
	}
}

func TestNormalizePatchPathStripsTimestampsAndQuotes(t *testing.T) {
	cases := map[string]string{
		"a/foo.txt\t2024-01-01 12:00:00.000000000 +0000": "foo.txt",
		"b/foo.txt 2024-01-01 12:00:00":                   "foo.txt",
		`"b/foo bar.txt"`: "foo bar.txt",
		"a/foo.txt":       "foo.txt",
	}
	for in, want := range cases {
		got := normalizePatchPath(in)
		if got != want {
			t.Fatalf("normalizePatchPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestApplyPatchHunksFuzzyPastEOF(t *testing.T) {
	original := []string{"package main", "", "func main() {", "}"}
	hunks := []patchHunk{{
		oldStart: 20, // stale line number well past EOF
		oldLines: []string{"func main() {", "}"},
		newLines: []string{"func main() {", "\t// hi", "}"},
	}}
	_, err := applyPatchHunks(original, hunks, false)
	if err == nil {
		t.Fatal("expected strict apply to fail with stale line numbers")
	}
	got, err := applyPatchHunks(original, hunks, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[3] != "\t// hi" {
		t.Fatalf("got %#v", got)
	}
}

func TestSchemaPatchExampleParsesCleanly(t *testing.T) {
	// Keep in sync with the single-file example in tools.go.
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n \n+// new comment\n func main() {\n }\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("parse failed: %+v", files)
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 4 || len(h.newLines) != 5 {
		t.Fatalf("old=%d new=%d: %#v %#v", len(h.oldLines), len(h.newLines), h.oldLines, h.newLines)
	}
}
