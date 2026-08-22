package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestListFilesRecursive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sub", "b.go"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ListFiles(context.Background(), ".", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/a.go") || !strings.Contains(out, "pkg/sub/b.go") {
		t.Fatalf("unexpected listing: %q", out)
	}
}

func TestListFilesSubdirWorkspaceRelative(t *testing.T) {
	dir := t.TempDir()
	web := filepath.Join(dir, "internal", "server", "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)

	out, err := exec.ListFiles(context.Background(), "internal", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "internal/server/web/index.html") {
		t.Fatalf("expected workspace-relative path, got: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "server/" || line == "server/web/" || line == "server/web/index.html" {
			t.Fatalf("subdir-relative path leaked into listing: %q\nfull:\n%s", line, out)
		}
	}

	out, err = exec.ListFiles(context.Background(), "internal", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "internal/server/") {
		t.Fatalf("expected workspace-relative dir entry, got: %q", out)
	}
	if strings.Contains(out, "\nserver/") || out == "server/" || strings.HasPrefix(out, "server/") {
		t.Fatalf("subdir-relative dir entry leaked: %q", out)
	}
}

func TestGlobFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.GlobFiles(context.Background(), "*.go", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") || strings.Contains(out, "readme.txt") {
		t.Fatalf("unexpected glob: %q", out)
	}
}

// TestGlobFilesBareGlobstar pins the lone-"**" semantics: it means
// "everything", not "nothing" (the regex translation's leading-** branch
// used to render it as ^(?:.*/)?$, which matches no files).
func TestGlobFilesBareGlobstar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.go"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.GlobFiles(context.Background(), "**", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "sub/deep.go") {
		t.Fatalf("bare ** must match files at any depth, got: %q", out)
	}
}

// TestGlobFilesHiddenFiles pins the glob tool's discovery semantics: as the
// name-based discovery tool it must match dotfiles (e.g. .env via "*.env"),
// while hidden directories stay pruned (reach them via the path argument).
func TestGlobFilesHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte("name: ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)

	// Dotfiles are visible to the name-based discovery tool.
	out, err := exec.GlobFiles(context.Background(), "*.env", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".env") {
		t.Fatalf("expected .env to match *.env, got %q", out)
	}

	// Hidden directories are still pruned: *.yml must not reach .github/.
	out, err = exec.GlobFiles(context.Background(), "*.yml", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".github") || strings.Contains(out, "ci.yml") {
		t.Fatalf("hidden dir should be pruned, got %q", out)
	}

	// ...but passing the hidden dir as the path argument reaches inside it.
	out, err = exec.GlobFiles(context.Background(), "*.yml", ".github", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".github/workflows/ci.yml") {
		t.Fatalf("expected path-scoped glob into hidden dir, got %q", out)
	}
}

// TestFilterByTrackedSet verifies the tracked_only filter contract: only paths
// present in the tracked set survive, and a tree with no tracked matches yields
// an empty result rather than silently falling back to the unfiltered list.
func TestFilterByTrackedSet(t *testing.T) {
	tracked := map[string]struct{}{
		"a.go":   {},
		"b/c.go": {},
	}

	// No tracked files known: empty result, never the original list.
	if got := filterByTrackedSet([]string{"x.go"}, nil); got != nil {
		t.Fatalf("empty tracked set: got %v, want nil", got)
	}
	if got := filterByTrackedSet(nil, tracked); got != nil {
		t.Fatalf("empty paths: got %v, want nil", got)
	}

	// All paths tracked: everything survives.
	got := filterByTrackedSet([]string{"a.go", "b/c.go"}, tracked)
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b/c.go" {
		t.Fatalf("all tracked: got %v, want [a.go b/c.go]", got)
	}

	// Partial match: only tracked paths survive.
	got = filterByTrackedSet([]string{"a.go", "untracked.go"}, tracked)
	if len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("partial match: got %v, want [a.go]", got)
	}

	// No match: must return empty, not the unfiltered list (this previously
	// surfaced ignored/untracked files under tracked_only=true).
	if got := filterByTrackedSet([]string{"untracked.go", "junk.txt"}, tracked); got != nil {
		t.Fatalf("no match: got %v, want nil (must not show untracked files)", got)
	}
}

func TestReadFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFiles([]string{"a.txt", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "=== a.txt ===") || !strings.Contains(out, "alpha") {
		t.Fatalf("missing a.txt content: %q", out)
	}
	if !strings.Contains(out, "=== b.txt ===") || !strings.Contains(out, "beta") {
		t.Fatalf("missing b.txt content: %q", out)
	}
}

// TestReadFilesParallelOrder pins the parallel read_files behavior: all files
// are present and headers appear in input order, so tool output stays
// deterministic (prompt-cache stable) despite concurrent reads. A 12-file
// batch exercises the bounded worker pool; run under -race to catch
// concurrent result writes.
func TestReadFilesParallelOrder(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content-"+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFiles(names)
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, name := range names {
		idx := strings.Index(out, "=== "+name+" ===")
		if idx < 0 {
			t.Fatalf("missing header for %s in output", name)
		}
		if idx <= last {
			t.Fatalf("headers out of order for %s", name)
		}
		last = idx
		if !strings.Contains(out, "content-"+name) {
			t.Fatalf("missing content for %s", name)
		}
	}
}

// TestReadFilesParallelError preserves sequential error semantics: the first
// failing path (in input order) is reported, even though later paths may have
// completed concurrently.
func TestReadFilesParallelError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	_, err := exec.ReadFiles([]string{"a.txt", "missing.txt", "b.txt"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("expected error to name missing file, got %v", err)
	}
}

// TestReadFilesTruncationRuneSafe pins the multi-file truncation to a UTF-8
// rune boundary: the byte cap can land mid-rune, and the cut must back off
// (contextmgr.TruncateRuneSafe) instead of emitting invalid UTF-8 that would
// be U+FFFD-replaced when JSON-encoded to the model.
func TestReadFilesTruncationRuneSafe(t *testing.T) {
	dir := t.TempDir()
	// "=== a.txt ===\n" is 14 bytes. Fill the first file so the second
	// block's cut (remain = readFilesMaxTotalBytes - total = 61) lands one
	// byte into a 3-byte rune (界): 15 bytes of block prefix
	// ("\n=== b.txt ===\n") + 46 content bytes = 15 full runes + 1 partial.
	contentA := strings.Repeat("a", readFilesMaxTotalBytes-14-61)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(contentA), 0o644); err != nil {
		t.Fatal(err)
	}
	contentB := strings.Repeat("界", 100) // 300 bytes
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte(contentB), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	out, err := exec.ReadFiles([]string{"a.txt", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("output contains invalid UTF-8 (mid-rune truncation): …%q", out[len(out)-80:])
	}
	if !strings.Contains(out, "=== b.txt ===") {
		t.Fatal("second file header missing")
	}
	// Rune-safe backoff keeps exactly 15 full 界 runes (45 bytes), not 16.
	if !strings.Contains(out, strings.Repeat("界", 15)) {
		t.Fatal("expected 15 multibyte runes kept")
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("expected truncation marker")
	}
}

// TestGlobRegexCacheBoundedAndReusable verifies the glob regex cache never
// grows beyond globRegexCacheMax and that compiles are reused (a repeated
// pattern does not require recompilation). This is a regression guard for a
// previous design that reassigned the package-level map variable under
// concurrency, which raced with concurrent readers and discarded in-flight
// stores, violating the cap.
func TestGlobRegexCacheBoundedAndReusable(t *testing.T) {
	// Start from a clean cache.
	globRegexMu.Lock()
	resetGlobRegexCacheLocked()
	globRegexMu.Unlock()

	// Prime the cache with distinct patterns.
	patterns := make([]string, 0, globRegexCacheMax+50)
	for i := 0; i < globRegexCacheMax+50; i++ {
		patterns = append(patterns, "dir"+itoa(i)+"/**/*.go")
	}
	for _, p := range patterns {
		// A path that won't match is fine; we only care about compilation.
		matchGlobRegex(p, "dir/file.go")
	}

	globRegexMu.Lock()
	size := len(globRegexCache)
	globRegexMu.Unlock()
	if size > globRegexCacheMax {
		t.Fatalf("glob regex cache over cap: got %d, max %d", size, globRegexCacheMax)
	}

	// Re-run the last distinct pattern; the cache should reuse the stored
	// regex. We assert reuse by checking that the entry is present after the
	// run (regression for the reset that discarded concurrent stores).
	last := patterns[len(patterns)-1]
	matchGlobRegex(last, "dir/file.go")
	globRegexMu.Lock()
	_, present := globRegexCache[last]
	globRegexMu.Unlock()
	if !present {
		t.Fatalf("expected pattern %q to be cached after reuse", last)
	}
}

// TestGlobRegexCacheConcurrent exercises the cache under concurrency to guard
// against the former variable-reassignment race (would surface under -race).
func TestGlobRegexCacheConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				pat := "g" + itoa(seed) + "/**/*." + itoa(i%4)
				matchGlobRegex(pat, "g/x.txt")
			}
		}(g)
	}
	wg.Wait()

	globRegexMu.Lock()
	size := len(globRegexCache)
	globRegexMu.Unlock()
	if size > globRegexCacheMax {
		t.Fatalf("glob regex cache over cap after concurrent use: got %d, max %d", size, globRegexCacheMax)
	}
}

// itoa is a small strconv.Itoa-free helper to keep this test free of an extra
// import churn; it handles non-negative ints only.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestMatchGlobPatternRejectsMalformedGlob guards against a previous basename
// branch that, on filepath.ErrBadPattern, fell back to a substring test
// (strings.Contains with the pattern minus a leading "*"). That silently
// redefined malformed globs as matches. Both basename and path branches must
// now treat a bad pattern as no-match.
func TestMatchGlobPatternRejectsMalformedGlob(t *testing.T) {
	// A bare "[" is a malformed character class -> filepath.ErrBadPattern.
	// Must not match anything and must not panic.
	if matchGlobPattern("[", "foo.txt") {
		t.Fatalf(`malformed basename glob "[" matched; expected no match`)
	}
	if matchGlobPattern("[unclosed", "foo.txt") {
		t.Fatalf(`malformed basename glob "[unclosed" matched; expected no match`)
	}
	// Path-based malformed glob (contains "/") should also be no-match.
	if matchGlobPattern("pkg/[bad", "pkg/foo.txt") {
		t.Fatalf(`malformed path glob "pkg/[bad" matched; expected no match`)
	}
}

// resetGlobRegexCacheLocked clears the glob regex cache and insertion-order
// slice. Caller must hold globRegexMu.
//
// Lives in this _test.go file: production code mutates the cache through its
// own paths; only tests need wholesale resets.
func resetGlobRegexCacheLocked() {
	globRegexCache = make(map[string]*regexp.Regexp)
	globRegexOrder = globRegexOrder[:0]
}
