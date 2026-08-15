package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecurePathAbsoluteWithinWorkingDir(t *testing.T) {
	dir := t.TempDir()
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)

	got, err := exec.SecurePath(readme)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(readme)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSecurePathAbsoluteWithoutLeadingSlash(t *testing.T) {
	dir := t.TempDir()
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)

	withoutSlash := strings.TrimPrefix(filepath.ToSlash(readme), "/")
	got, err := exec.SecurePath(withoutSlash)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(readme)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSecurePathRelative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	got, err := exec.SecurePath("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSecurePathBlocksSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	exec := NewExecutor(root)
	_, err := exec.ReadFileRange(filepath.Join("escape", "secret.txt"), 0, 0, "", false)
	if err == nil {
		t.Fatal("expected symlink escape to be blocked")
	}
	if !strings.Contains(err.Error(), "outside of allowed boundary") && !strings.Contains(err.Error(), "outside of working directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")
	exec := NewExecutor(dir)
	if err := exec.WriteFile("nested/file.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}
}

func TestReplaceInFileUniqueMatchReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	content := "foo bar baz\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	if _, err := exec.ReplaceInFile("a.txt", "foo", "qux", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "qux bar baz\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceInFileAmbiguousRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	content := "foo bar foo\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	// Multiple matches without replace_all must be refused, not silently
	// replaced at the first occurrence.
	if _, err := exec.ReplaceInFile("a.txt", "foo", "baz", false); err == nil || !strings.Contains(err.Error(), "appears 2 times") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("file must be unchanged after refused ambiguity, got %q", got)
	}
	// replace_all=true replaces all occurrences.
	if _, err := exec.ReplaceInFile("a.txt", "foo", "baz", true); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "baz bar baz\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceInFileNotFoundClosestMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	content := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	// A typo in the search block ("packge"): the error must point at the
	// closest line so the model can fix the block in one retry.
	_, err := exec.ReplaceInFile("main.go", "packge main", "package main", false)
	if err == nil || !strings.Contains(err.Error(), "closest match at line 1") {
		t.Fatalf("expected closest-match diagnostic, got %v", err)
	}
	// Multi-line search miss: the first non-empty line anchors the report.
	_, err = exec.ReplaceInFile("main.go", "func main( {\n}", "x", false)
	if err == nil || !strings.Contains(err.Error(), "closest match at line 3") {
		t.Fatalf("expected closest-match diagnostic for multi-line miss, got %v", err)
	}
	// replace_all=true reports the same diagnostic.
	_, err = exec.ReplaceInFile("main.go", "nope", "x", true)
	if err == nil || !strings.Contains(err.Error(), "closest match at line 1") {
		t.Fatalf("expected closest-match diagnostic in replace_all mode, got %v", err)
	}
}

func TestPatchFileApply(t *testing.T) {
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
		"+// patched\n" +
		" func main() {\n" +
		" }\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	msg, err := exec.PatchFile(context.Background(), diff, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "main.go") {
		t.Fatalf("unexpected message: %s", msg)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// patched") {
		t.Fatalf("patch not applied: %q", got)
	}
}

func TestPatchFileCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	diff := "" +
		"--- /dev/null\n" +
		"+++ b/new.txt\n" +
		"@@ -0,0 +1,2 @@\n" +
		"+hello\n" +
		"+world\n"

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\nworld\n" {
		t.Fatalf("got %q", got)
	}
}
