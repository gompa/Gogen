package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	out, err := exec.ListFiles(".", true, false)
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

	out, err := exec.ListFiles("internal", true, false)
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

	out, err = exec.ListFiles("internal", false, false)
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
	out, err := exec.GlobFiles("*.go", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") || strings.Contains(out, "readme.txt") {
		t.Fatalf("unexpected glob: %q", out)
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
