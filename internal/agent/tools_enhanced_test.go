package agent

import (
	"context"
	"fmt"
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
	out, err := exec.ReadFileRange("lines.txt", 2, 2, "", false)
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

// TestReadFileRangeOffsetWithoutLimitCapsAtMaxLines pins the read cap: an
// offset read with no limit must stop at readFileMaxLines (10k), not run to
// EOF — the old `offset == 0` guard let "read from line N with no limit"
// return an unbounded result for huge files.
func TestReadFileRangeOffsetWithoutLimitCapsAtMaxLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	var b strings.Builder
	const total = readFileMaxLines + 500
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "line-%04d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange("big.txt", 100, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	// The header must show a truncated range (end < total), and the body must
	// contain the offset start plus exactly the capped number of lines.
	if !strings.Contains(out, "Lines 100-10099 of 10500") {
		t.Fatalf("expected truncated range header, got %q", firstLine(out))
	}
	if !strings.Contains(out, "line-0099") || !strings.Contains(out, "line-10098") {
		t.Fatalf("expected lines 100..10099 in the body")
	}
	if strings.Contains(out, "line-10099") {
		t.Fatalf("read past the cap: body contains line 10099")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestReadFileRangeLineNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	content := "one\ntwo\nthree\nfour\nfive\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange("lines.txt", 2, 2, "", true)
	if err != nil {
		t.Fatal(err)
	}
	// Line numbers should start at 2 (offset), not 1
	if !strings.Contains(out, "2: two") || !strings.Contains(out, "3: three") {
		t.Fatalf("expected line numbers, got %q", out)
	}
	// Should not contain lines 1 or 4
	if strings.Contains(out, "1: one") || strings.Contains(out, "4: four") {
		t.Fatalf("should not contain lines outside range, got %q", out)
	}
}

func TestReadFileRangeLineNumbersAlignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	// Create a file with enough lines to test alignment
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange("lines.txt", 95, 6, "", true)
	if err != nil {
		t.Fatal(err)
	}
	// Numbers should be right-aligned (3 digits for 100)
	if !strings.Contains(out, " 95: line 95") || !strings.Contains(out, "100: line 100") {
		t.Fatalf("expected right-aligned line numbers, got %q", out)
	}
}

func TestReadFileRangeLineNumbersWithSearch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	content := "apple\nbanana\ncherry\ndate\nelderberry\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	// Search for "cherry", with 1 line context
	out, err := exec.ReadFileRange("lines.txt", 1, 3, "cherry", true)
	if err != nil {
		t.Fatal(err)
	}
	// Line numbers should reflect actual file positions (2-4)
	if !strings.Contains(out, "2: banana") || !strings.Contains(out, "3: cherry") || !strings.Contains(out, "4: date") {
		t.Fatalf("expected actual line numbers with search, got %q", out)
	}
}

// TestReadFileRangeSearchWindowCappedByLimit pins the search-mode window
// semantics: limit caps the total lines returned (before + match + after), and
// offset > 0 sets the context lines before the match while limit still caps the
// total. Regression for the old ctx = limit/2 derivation, which returned
// limit+1 lines for even limits and let offset silently override limit.
func TestReadFileRangeSearchWindowCappedByLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "search.txt")
	// 41 lines; the search match lives at line 21, with 20 lines on each side.
	var lines []string
	for i := 1; i <= 41; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	tests := []struct {
		name      string
		offset    int
		limit     int
		wantTotal int
		wantFirst string
		wantLast  string
	}{
		{
			name:      "limit only, even limit caps at limit (was limit+1)",
			offset:    0,
			limit:     10,
			wantTotal: 10,
			wantFirst: "line 16", // 5 before + match
			wantLast:  "line 25", // 4 after
		},
		{
			name:      "even limit 2 caps at 2",
			offset:    0,
			limit:     2,
			wantTotal: 2,
			wantFirst: "line 20",
			wantLast:  "line 21",
		},
		{
			name:      "limit 1 returns only the match",
			offset:    0,
			limit:     1,
			wantTotal: 1,
			wantFirst: "line 21",
			wantLast:  "line 21",
		},
		{
			name:      "limit larger than file returns whole file",
			offset:    0,
			limit:     41,
			wantTotal: 41,
			wantFirst: "line 01",
			wantLast:  "line 41",
		},
		{
			name:      "offset sets before, limit caps total",
			offset:    2,
			limit:     10,
			wantTotal: 10,
			wantFirst: "line 19",
			wantLast:  "line 28",
		},
		{
			name:      "offset larger than limit budget is clamped",
			offset:    20,
			limit:     10,
			wantTotal: 10,
			wantFirst: "line 12", // 9 before + match
			wantLast:  "line 21",
		},
		{
			name:      "offset only keeps default 10 after",
			offset:    20,
			limit:     0,
			wantTotal: 31, // 20 before + match + 10 after
			wantFirst: "line 01",
			wantLast:  "line 31",
		},
		{
			name:      "defaults 10 before and after",
			offset:    0,
			limit:     0,
			wantTotal: 21,
			wantFirst: "line 11",
			wantLast:  "line 31",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.ReadFileRange(path, tc.offset, tc.limit, "^line 21$", false)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(out, "\n")
			body := parts[1:] // first line is the "Lines X-Y of Z" header
			if len(body) != tc.wantTotal {
				t.Fatalf("window = %d lines, want %d\nout: %q", len(body), tc.wantTotal, out)
			}
			if body[0] != tc.wantFirst || body[len(body)-1] != tc.wantLast {
				t.Fatalf("window = [%q .. %q], want [%q .. %q]\nout: %q",
					body[0], body[len(body)-1], tc.wantFirst, tc.wantLast, out)
			}
		})
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
	out, err := exec.ReadFileRange("big.txt", 0, 0, "", false)
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

func TestGitLog(t *testing.T) {
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
}

func TestPlanModeAllowsGitTools(t *testing.T) {
	a := &Agent{Mode: ModePlan}
	allowed := a.AllowedToolNames()
	for _, tool := range []string{"find_symbol", "git"} {
		if _, ok := allowed[tool]; !ok {
			t.Fatalf("%s should be allowed in plan mode", tool)
		}
	}
}

// TestMergedToolHandlersRejectUnknownActions pins the action/kind routing of
// the consolidated tools (git, find_symbol, background_job): unknown enum
// values fail fast with a clear message instead of falling through to the
// executors.
func TestMergedToolHandlersRejectUnknownActions(t *testing.T) {
	a := &Agent{Executor: &Executor{WorkingDir: t.TempDir()}}
	cases := []struct {
		name string
		call func() (string, error)
		want string
	}{
		{"git", func() (string, error) {
			return handleGit(context.Background(), a, map[string]interface{}{"action": "bogus"})
		}, "unknown git action"},
		{"find_symbol", func() (string, error) {
			return handleFindSymbol(context.Background(), a, map[string]interface{}{"kind": "bogus", "symbol": "x"})
		}, "unknown find_symbol kind"},
		{"background_job", func() (string, error) {
			return handleBackgroundJob(context.Background(), a, map[string]interface{}{"action": "bogus", "job_id": "j1"})
		}, "unknown background_job action"},
	}
	for _, tc := range cases {
		_, err := tc.call()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: expected error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// TestReadFileRangeOffsetPastEnd pins the past-EOF behavior of offset reads:
// an offset beyond the last line must report it (the previous behavior
// returned an empty result that was indistinguishable from an empty file).
func TestReadFileRangeOffsetPastEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lines.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange("lines.txt", 4, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "File has 3 lines; offset 4 is past end.") {
		t.Fatalf("expected past-end message, got %q", out)
	}
	out, err = exec.ReadFileRange("empty.txt", 1, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "File has 0 lines; offset 1 is past end.") {
		t.Fatalf("expected past-end message for empty file, got %q", out)
	}
}
