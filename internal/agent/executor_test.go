package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExecuteCommandPreservesOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "echo hello && exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected command output in result, got %q", out)
	}
}

func TestExecuteCommandPreservesOutputOnTimeout(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, err := exec.ExecuteCommand(ctx, "echo partial && sleep 2")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("expected partial output before timeout, got %q", out)
	}
}

func TestExecuteCommandSuccess(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "echo ok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestExecuteCommandCancelStopsChildren(t *testing.T) {
	dir := t.TempDir()
	ex := NewExecutor(dir)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		// echo keeps sh alive as parent of sleep; without process-group kill
		// CombinedOutput hangs after cancel with children holding pipes.
		out, err = ex.ExecuteCommand(ctx, "echo partial && sleep 30")
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteCommand hung after cancel")
	}
	if err == nil {
		t.Fatal("expected cancel error")
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("expected partial output before cancel, got %q", out)
	}
}

func TestExecuteCommandCancelPipeline(t *testing.T) {
	dir := t.TempDir()
	ex := NewExecutor(dir)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = ex.ExecuteCommand(ctx, "sleep 30 | cat")
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline ExecuteCommand hung after cancel")
	}
}

func TestExecuteCommandUsesWorkingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("found"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "cat marker.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "found" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestExecuteCommandBwrapMissingErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		// bwrap is Unix-only: Windows rejects the sandbox in
		// checkCommandConfig before any PATH lookup, with a different
		// error message.
		t.Skip("bwrap sandbox is not supported on Windows")
	}
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetSandbox("bwrap")
	// Force LookPath failure by using a PATH without bwrap.
	t.Setenv("PATH", dir)

	_, err := exec.ExecuteCommand(context.Background(), "echo hi")
	if err == nil {
		t.Fatal("expected error when bwrap is missing")
	}
	if !strings.Contains(err.Error(), "bwrap not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteCommandUnknownSandboxErrors(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetSandbox("docker")

	_, err := exec.ExecuteCommand(context.Background(), "echo hi")
	if err == nil {
		t.Fatal("expected error for unknown sandbox")
	}
	if !strings.Contains(err.Error(), "unknown command_sandbox") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteCommandStreamsOutputToSink(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	var mu sync.Mutex
	var gotCmd string
	var chunks []string
	sink := func(command, chunk string) {
		mu.Lock()
		defer mu.Unlock()
		if gotCmd == "" {
			gotCmd = command
		}
		chunks = append(chunks, chunk)
	}

	out, err := exec.ExecuteCommand(
		ContextWithToolOutput(context.Background(), sink),
		"printf 'one\\ntwo\\n' && echo err >&2",
	)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	joined := strings.Join(chunks, "")
	cmd := gotCmd
	mu.Unlock()

	if cmd != "printf 'one\\ntwo\\n' && echo err >&2" {
		t.Fatalf("sink received wrong command: %q", cmd)
	}
	// Chunk boundaries are pipe-dependent, but the concatenation must be
	// byte-identical to the returned (accumulated) output, and both stdout
	// and stderr must be present.
	if joined != out {
		t.Fatalf("sink output %q != returned output %q", joined, out)
	}
	if !strings.Contains(joined, "one\ntwo") || !strings.Contains(joined, "err") {
		t.Fatalf("sink output missing stdout/stderr content: %q", joined)
	}
}

// TestExecuteCommandOutputBounded pins the in-memory output cap: a command
// producing more than the configured cap returns the first cap bytes plus
// the standard truncation marker (same prefix the context manager uses, so
// its later pass does not double-mark), while the live sink still receives
// every chunk uncapped.
func TestExecuteCommandOutputBounded(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetMaxToolOutputBytes(64)

	var mu sync.Mutex
	var joined strings.Builder
	sink := func(_ string, chunk string) {
		mu.Lock()
		joined.WriteString(chunk)
		mu.Unlock()
	}

	// Deterministic 100-byte payload: 10 x 10 chars.
	cmd := "seq 1 10 | awk '{printf \"0123456789\"}'"
	out, err := exec.ExecuteCommand(
		ContextWithToolOutput(context.Background(), sink),
		cmd,
	)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	sinkOut := joined.String()
	mu.Unlock()
	if len(sinkOut) != 100 {
		t.Fatalf("sink received %d bytes, want the full 100 (sink is never capped)", len(sinkOut))
	}
	if len(out) > 64 {
		t.Fatalf("capped output = %d bytes, want <= 64", len(out))
	}
	// The marker length is reserved inside the cap, so the payload prefix
	// ends before it; the FIRST bytes must still be preserved.
	if !strings.HasPrefix(out, "0123456789012345") {
		t.Fatalf("capped output must keep the FIRST bytes, got %q", out)
	}
	if !strings.Contains(out, "\n… truncated (") {
		t.Fatalf("capped output must carry the standard truncation marker, got %q", out)
	}

	// 0 = explicitly uncapped: byte-identical to the sink.
	exec.SetMaxToolOutputBytes(0)
	out2, err := exec.ExecuteCommand(
		ContextWithToolOutput(context.Background(), sink),
		cmd,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) != 100 {
		t.Fatalf("uncapped output = %d bytes, want the full 100", len(out2))
	}
}

func TestExecuteCommandSinkReceivesPartialOutputOnTimeout(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	var mu sync.Mutex
	joined := ""
	sink := func(_ string, chunk string) {
		mu.Lock()
		joined += chunk
		mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(
		ContextWithToolOutput(context.Background(), sink),
		50*time.Millisecond,
	)
	defer cancel()

	out, err := exec.ExecuteCommand(ctx, "echo partial && sleep 2")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	sinkOut := joined
	mu.Unlock()
	if !strings.Contains(out, "partial") || !strings.Contains(sinkOut, "partial") {
		t.Fatalf("expected partial output in both return and sink; out=%q sink=%q", out, sinkOut)
	}
}

func TestExecuteCommandNoSinkMatchesReturnedOutput(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)

	out, err := exec.ExecuteCommand(context.Background(), "printf 'x\\ny\\n' && echo z >&2")
	if err != nil {
		t.Fatal(err)
	}
	if out != "x\ny\nz\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestLargeFileSearchSkipsTotalDrain verifies readWithRegexSearch skips the
// end-of-file drain for files larger than searchMaxFileBytes and reports a
// lower-bound total instead of reading the whole tail just to count lines.
func TestLargeFileSearchSkipsTotalDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("needle-here\n"); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x\n", 4096)
	for i := 0; i < 128; i++ {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	exec := NewExecutor(dir)
	out, err := exec.ReadFileRange(path, 0, 10, "needle-here", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total line count omitted") {
		t.Fatalf("expected lower-bound header for large file, got: %q", out)
	}
	if !strings.Contains(out, "needle-here") {
		t.Fatalf("expected match content in output, got: %q", out)
	}
}

// TestReadFileRangeSearchWindowFillsLimit verifies that the search-mode
// window fills out to the requested limit: when the first regex match sits
// near the start of the file, the before-context ring holds fewer lines
// than the ctxBefore budget, and the unused before-budget must flow into
// the after-context instead of leaving the window under-filled.
func TestReadFileRangeSearchWindowFillsLimit(t *testing.T) {
	const totalFiller = 200
	writeNeedleFile := func(t *testing.T, dir string, matchLine int) string {
		t.Helper()
		path := filepath.Join(dir, "log.txt")
		var sb strings.Builder
		for i := 1; i <= totalFiller; i++ {
			if i == matchLine {
				sb.WriteString("needle\n")
				continue
			}
			fmt.Fprintf(&sb, "filler %d\n", i)
		}
		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name       string
		matchLine  int
		offset     int
		limit      int
		wantStart  int
		wantWindow int
	}{
		// Regression: with the match on line 2, 4 of the 5 before-budget
		// lines go unused; the window must still span the full limit of 11.
		{"near-start match fills unused before-budget into after", 2, 0, 11, 1, 11},
		// Match on the very first line: no before-context at all.
		{"first-line match grants entire budget to after", 1, 0, 11, 1, 11},
		// Ring fully filled: the budgeted 5/5 split of the 11-line window.
		{"full before-context keeps budgeted split", 50, 0, 11, 45, 11},
		// Explicit offset pins the before-context; after takes the remainder.
		{"offset-pinned before leaves remainder to after", 50, 3, 11, 47, 11},
		// No limit: the fixed default of 10 lines per side still applies.
		{"no limit keeps default 10-line sides", 2, 0, 0, 1, 12},
	}
	headerRe := regexp.MustCompile(`^Lines (\d+)-(\d+) of \d+\+? \(matched "needle" at line (\d+)\)$`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeNeedleFile(t, dir, tt.matchLine)
			exec := NewExecutor(dir)
			out, err := exec.ReadFileRange(path, tt.offset, tt.limit, "needle", true)
			if err != nil {
				t.Fatal(err)
			}
			header, body, _ := strings.Cut(out, "\n")
			m := headerRe.FindStringSubmatch(header)
			if m == nil {
				t.Fatalf("unexpected header: %q", header)
			}
			var start, end, gotMatch int
			fmt.Sscanf(m[1]+" "+m[2]+" "+m[3], "%d %d %d", &start, &end, &gotMatch)
			if gotMatch != tt.matchLine {
				t.Fatalf("matched line = %d, want %d (header %q)", gotMatch, tt.matchLine, header)
			}
			if start != tt.wantStart || end-start+1 != tt.wantWindow {
				t.Fatalf("window = lines %d-%d (%d lines), want start %d with %d lines (header %q)",
					start, end, end-start+1, tt.wantStart, tt.wantWindow, header)
			}
			if got := strings.Count(body, "\n") + 1; got != tt.wantWindow {
				t.Fatalf("body has %d lines, want %d", got, tt.wantWindow)
			}
			if !strings.Contains(body, "needle") {
				t.Fatalf("expected match content in body, got: %q", body)
			}
		})
	}
}

// TestOutputBufferTailRuneSafe pins the bounded-tail trim to a rune
// boundary: bytes.Buffer.Next is a byte cut, and when the overflow drops
// bytes up to the middle of a multi-byte rune, the retained tail must start
// at the next rune boundary instead of leading with an invalid partial
// character (every status/input result built from the buffer would
// otherwise start with invalid UTF-8).
func TestOutputBufferTailRuneSafe(t *testing.T) {
	var b outputBuffer
	// 7 ASCII bytes + 界 (bytes 7-9) + 8 ASCII bytes = 18 bytes; the raw
	// 10-byte cut (drop 8) lands inside the 界, so the rune-safe trim must
	// drop through byte 9 and keep only the trailing ASCII.
	b.append([]byte("abcdefg"), 10, nil)
	b.append([]byte("界"), 10, nil)
	b.append([]byte("hijklmno"), 10, nil)
	got := b.string()
	if got != "hijklmno" {
		t.Fatalf("retained tail = %q, want %q", got, "hijklmno")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("retained tail is not valid UTF-8: %q", got)
	}
	// drain returns the same rune-safe content to the input-delta path.
	if drained := b.drain(); drained != "hijklmno" || !utf8.ValidString(drained) {
		t.Fatalf("drained tail = %q, want rune-safe %q", drained, "hijklmno")
	}
}

// TestOutputBufferTailKeepsWithinCapUntouched verifies the trim is a no-op
// while the buffer fits within max, including a multi-byte rune at the end.
func TestOutputBufferTailKeepsWithinCapUntouched(t *testing.T) {
	var b outputBuffer
	payload := []byte("abcdefg界")
	b.append(payload, 10, nil)
	if got := b.string(); got != string(payload) {
		t.Fatalf("within-cap buffer = %q, want untouched %q", got, payload)
	}
}
