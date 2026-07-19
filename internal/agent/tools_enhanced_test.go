package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileRangeOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	content := "one\ntwo\nthree\nfour\nfive\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange("lines.txt", 2, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Lines 2-3 of 5") {
		t.Fatalf("expected range header, got %q", out)
	}
	if !strings.Contains(out, "two\nthree") {
		t.Fatalf("expected selected lines, got %q", out)
	}
}

func TestReadFileRangeSizeWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	data := strings.Repeat("x", readFileWarnBytes+1)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange("big.txt", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Warning: file is") {
		t.Fatalf("expected size warning, got %q", out)
	}
}

func TestFindReferences(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nfunc Target() {}\n\nfunc other() { Target() }\n"
	if err := os.WriteFile(filepath.Join(dir, "refs.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.FindReferences(context.Background(), "Target", "", "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "func Target()") || !strings.Contains(out, "Target()") {
		t.Fatalf("expected references, got %q", out)
	}
}

func TestPatchFileDryRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package main\n" +
		" \n" +
		"+// dry\n" +
		" func main() {}\n"

	exec := NewExecutor(dir)
	msg, err := exec.PatchFile(context.Background(), diff, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Dry run OK") || !strings.Contains(msg, "would change") {
		t.Fatalf("unexpected dry run message: %s", msg)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("dry run should not modify file, got %q", got)
	}
}

func TestGitLogAndBlame(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.go")
	runGit("commit", "-m", "initial commit")

	exec := NewExecutor(dir)
	logOut, err := exec.GitLog(context.Background(), "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logOut, "initial commit") {
		t.Fatalf("unexpected git log: %q", logOut)
	}

	blameOut, err := exec.GitBlame(context.Background(), "tracked.go", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blameOut, "package main") {
		t.Fatalf("unexpected git blame: %q", blameOut)
	}
}

func TestPlanModeAllowsGitTools(t *testing.T) {
	a := &Agent{Mode: ModePlan}
	allowed := a.AllowedToolNames()
	for _, tool := range []string{"find_references", "git_log", "git_blame"} {
		if _, ok := allowed[tool]; !ok {
			t.Fatalf("%s should be allowed in plan mode", tool)
		}
	}
}
