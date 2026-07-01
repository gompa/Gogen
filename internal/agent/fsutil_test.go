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

	got, err := exec.securePath(readme)
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
	got, err := exec.securePath(withoutSlash)
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
	got, err := exec.securePath("go.mod")
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
	_, err := exec.ReadFile(filepath.Join("escape", "secret.txt"))
	if err == nil {
		t.Fatal("expected symlink escape to be blocked")
	}
	if !strings.Contains(err.Error(), "outside of working directory") {
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

func TestReplaceInFileFirstOccurrenceOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	content := "foo bar foo\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	if _, err := exec.ReplaceInFile("a.txt", "foo", "baz", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "baz bar foo\n" {
		t.Fatalf("got %q", got)
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
	exec.RequireDeleteApproval = false
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
	exec.RequireDeleteApproval = false
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
