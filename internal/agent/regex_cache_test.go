package agent

import (
	"path"
	"regexp"
	"sync"
	"testing"
)

// TestCompiledRegexCachesAndBounded verifies the memo reuses compiled regexes
// and that the cache stays within its cap under FIFO eviction.
func TestCompiledRegexCachesAndBounded(t *testing.T) {
	regexMemoMu.Lock()
	resetRegexMemoLocked()
	regexMemoMu.Unlock()

	patterns := make([]string, 0, regexMemoMax+50)
	for i := 0; i < regexMemoMax+50; i++ {
		patterns = append(patterns, "pat"+itoa(i)+".go")
	}
	for _, p := range patterns {
		if _, err := compiledRegex(p); err != nil {
			t.Fatalf("compiledRegex(%q): %v", p, err)
		}
	}

	regexMemoMu.Lock()
	size := len(regexMemo)
	regexMemoMu.Unlock()
	if size > regexMemoMax {
		t.Fatalf("regex memo over cap: got %d, max %d", size, regexMemoMax)
	}

	// Re-run the last pattern; the entry must be present (reuse, not
	// eviction churn).
	last := patterns[len(patterns)-1]
	if _, err := compiledRegex(last); err != nil {
		t.Fatalf("compiledRegex(%q): %v", last, err)
	}
	regexMemoMu.Lock()
	_, present := regexMemo[last]
	regexMemoMu.Unlock()
	if !present {
		t.Fatalf("expected pattern %q to be cached after reuse", last)
	}
}

// TestCompiledRegexConcurrent exercises the memo under concurrency (would
// surface races under -race) and checks the cap holds.
func TestCompiledRegexConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := compiledRegex("g" + itoa(seed) + "_[0-9]+" + itoa(i%4)); err != nil {
					t.Errorf("compiledRegex: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	regexMemoMu.Lock()
	size := len(regexMemo)
	regexMemoMu.Unlock()
	if size > regexMemoMax {
		t.Fatalf("regex memo over cap after concurrent use: got %d, max %d", size, regexMemoMax)
	}
}

// TestCompiledRegexCompileErrorNotCached verifies invalid patterns return an
// error and are not stored in the memo.
func TestCompiledRegexCompileErrorNotCached(t *testing.T) {
	regexMemoMu.Lock()
	resetRegexMemoLocked()
	regexMemoMu.Unlock()

	bad := "unclosed["
	if _, err := compiledRegex(bad); err == nil {
		t.Fatalf("expected compile error for %q", bad)
	}
	regexMemoMu.Lock()
	_, present := regexMemo[bad]
	regexMemoMu.Unlock()
	if present {
		t.Fatalf("compile error for %q must not be cached", bad)
	}
}

// TestMatchGlobCachedMatchesPathMatch verifies the cached regex path is
// equivalent to path.Match for simple globs (no ** and no character
// classes), which is the precondition for routing them through the cache.
// path.Match (not filepath.Match) is the oracle: the cached matcher is
// slash-based by design (callers ToSlash the input), while on Windows
// filepath.Match additionally treats "/" as a separator, so
// filepath.Match("*", "bar/baz.go") is true there and would make the
// test platform-dependent.
func TestMatchGlobCachedMatchesPathMatch(t *testing.T) {
	patterns := []string{"*", "?", "a", "*.go", "a?c", "a*b*c", "src/*.go", "src/x_?.txt"}
	paths := []string{
		"a", "ab", "abc", "axxxbxxx c", "foo.go", "bar/baz.go",
		"src/main.go", "src/x_1.txt", "src/x_12.txt", "sub/src/main.go",
		"a1", "a2b", "aX",
	}
	for _, p := range patterns {
		for _, candidate := range paths {
			want, err := path.Match(p, candidate)
			if err != nil {
				t.Fatalf("path.Match(%q, %q): %v", p, candidate, err)
			}
			if got := matchGlobCached(p, candidate); got != want {
				t.Errorf("matchGlobCached(%q, %q) = %v, path.Match = %v", p, candidate, got, want)
			}
		}
	}
}

// TestMatchGlobPatternCharacterClassFallback verifies globs containing
// character classes still go through filepath.Match (exact semantics,
// including valid classes).
func TestMatchGlobPatternCharacterClassFallback(t *testing.T) {
	if !matchGlobPattern("f[ao]o.txt", "fao.txt") {
		t.Fatalf(`character-class glob "f[ao]o.txt" should match "fao.txt"`)
	}
	if !matchGlobPattern("f[ao]o.txt", "foo.txt") {
		t.Fatalf(`character-class glob "f[ao]o.txt" should match "foo.txt"`)
	}
	if matchGlobPattern("f[ao]o.txt", "fxx.txt") {
		t.Fatalf(`character-class glob "f[ao]o.txt" should not match "fxx.txt"`)
	}
}

// resetRegexMemoLocked clears the compiled regex memo. Callers must hold
// regexMemoMu.
//
// Lives in this _test.go file: only tests need wholesale memo resets;
// production code mutates the memo through compiledRegex's own eviction.
func resetRegexMemoLocked() {
	regexMemo = make(map[string]*regexp.Regexp)
	regexMemoOrder = regexMemoOrder[:0]
}
