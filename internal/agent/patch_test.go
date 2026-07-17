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
