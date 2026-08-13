package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceInFileReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	content := "foo bar foo\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	if _, err := exec.ReplaceInFile("a.txt", "foo", "baz", true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "baz bar baz\n" {
		t.Fatalf("got %q", got)
	}
}

// TestReplaceInFileEmptySearchRejected guards against empty-search corruption:
// strings.Count/Index/ReplaceAll treat "" as "match at every position", which
// would rewrite the file (interleaving the replacement between every rune in
// replace-all mode, or silently prepending in single mode). An empty search
// must error up front and leave the file untouched.
func TestReplaceInFileEmptySearchRejected(t *testing.T) {
	for _, replaceAll := range []bool{false, true} {
		name := "single"
		if replaceAll {
			name = "replace_all"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.txt")
			content := "foo bar foo\n"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			exec := NewExecutor(dir)
			if _, err := exec.ReplaceInFile("a.txt", "", "X", replaceAll); err == nil {
				t.Fatal("expected error for empty search string")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != content {
				t.Fatalf("file was modified: got %q, want %q", got, content)
			}
		})
	}
}

func TestPatchFileFuzzyRelocatesHunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "// header added\npackage main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		" \n" +
		"+// inserted\n" +
		" func main() {\n" +
		" }\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.PatchFile(context.Background(), diff, false, false)
	if err == nil {
		t.Fatal("expected strict patch to fail when header line is stale")
	}

	_, err = exec.PatchFile(context.Background(), diff, false, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// inserted") {
		t.Fatalf("fuzzy patch not applied: %q", got)
	}
}

func TestPatchFileValidatesAllBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.go"), []byte("package ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := "" +
		"--- a/ok.go\n+++ b/ok.go\n@@ -1 +1,2 @@\n package ok\n+// ok\n" +
		"--- a/bad.go\n+++ b/bad.go\n@@ -1 +1,2 @@\n package missing\n+// bad\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.PatchFile(context.Background(), diff, false, false)
	if err == nil {
		t.Fatal("expected failure")
	}

	got, err := os.ReadFile(filepath.Join(dir, "ok.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "// ok") {
		t.Fatal("ok.go should not have been modified when bad.go fails")
	}
}

func TestDetectProjectProfileGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := DetectProjectProfile(dir, "", "")
	if !strings.Contains(out, "go.mod") {
		t.Fatalf("missing go.mod marker: %s", out)
	}
	if !strings.Contains(out, "go test ./...") {
		t.Fatalf("missing default test command: %s", out)
	}
	if !strings.Contains(out, "internal/") {
		t.Fatalf("missing layout: %s", out)
	}
}

func TestPatchFileCRLFLineEndings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Diff uses CRLF line endings, as might come from a Windows LLM response.
	diff := "--- a/main.go\r\n" +
		"+++ b/main.go\r\n" +
		"@@ -1,4 +1,5 @@\r\n" +
		" package main\r\n" +
		" \r\n" +
		"+// crlf\r\n" +
		" func main() {\r\n" +
		" }\r\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.PatchFile(context.Background(), diff, false, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// crlf") {
		t.Fatalf("CRLF patch not applied: %q", got)
	}
}

func TestPatchFileOnDiskCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	// File on disk uses CRLF (Windows checkout), patch uses LF.
	original := "package main\r\n\r\nfunc main() {\r\n}\r\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		" \n" +
		"+// from-lf-patch\n" +
		" func main() {\n" +
		" }\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.PatchFile(context.Background(), diff, false, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// from-lf-patch") {
		t.Fatalf("on-disk CRLF file not patched: %q", got)
	}
}

func TestPatchFileRejectsEmptyHunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// Malformed @@ without recognizable -/+ sections should not silently succeed.
	diff := "--- a/main.go\n+++ b/main.go\n@@ broken @@\n package main\n"
	exec := NewExecutor(dir)
	_, err := exec.PatchFile(context.Background(), diff, false, true)
	if err == nil {
		t.Fatal("expected malformed hunk header to fail")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("file should be unchanged: %q", got)
	}
}

func TestPatchFileTrailingWhitespaceTolerance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n\t// code\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Diff context lines have trailing spaces (common LLM artifact).
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -2,3 +2,4 @@\n" +
		" \n" +
		" func main() {  \n" + // trailing spaces
		" \t// code  \n" + // leading space for context, trailing spaces artifact
		"+\t// new\n" +
		" }\n"

	// Should fail without fuzzy (exact mismatch on trailing spaces).
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.PatchFile(context.Background(), diff, false, false)
	if err == nil {
		t.Fatal("expected strict patch to fail when context has trailing whitespace")
	}

	// Should succeed with fuzzy (whitespace-tolerant matching).
	_, err = exec.PatchFile(context.Background(), diff, false, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// new") {
		t.Fatalf("fuzzy whitespace-tolerant patch not applied: %q", got)
	}
}

func TestPatchFileRejectsDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		" \n" +
		"+// first\n" +
		" func main() {\n" +
		" }\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		" \n" +
		"+// second\n" +
		" func main() {\n" +
		" }\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.PatchFile(context.Background(), diff, false, false)
	if err == nil {
		t.Fatal("expected error for duplicate patch target")
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(got) != original {
		t.Fatalf("file should be unchanged on duplicate reject: %q", got)
	}
}
