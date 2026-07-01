package agent

import (
	"testing"
)

// Sample DuckDuckGo HTML search response mirroring the actual DOM:
// <div class="result results_links results_links_deep web-result ">
//   <h2 class="result__title">
//     <a class="result__a" href="REDIRECT_URL">Title</a>
//   </h2>
//   <a class="result__snippet" href="REDIRECT_URL">Snippet <b>with</b> bold</a>
// </div>

const ddgHTMLFixture = `<!DOCTYPE html>
<html><head><title>DuckDuckGo</title></head><body>
<div class="results">
  <div class="result results_links results_links_deep web-result ">
    <h2 class="result__title">
      <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F">The Go Programming Language</a>
    </h2>
    <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F">Go is an open source programming language that makes it easy to build simple, reliable, and efficient software.</a>
  </div>
  <div class="result results_links results_links_deep web-result ">
    <h2 class="result__title">
      <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fstd">Go standard library &amp; packages</a>
    </h2>
    <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fstd">Package documentation for Go&#39;s standard library.</a>
  </div>
  <div class="result results_links results_links_deep web-result ">
    <h2 class="result__title">
      <a class="result__a" href="https://en.wikipedia.org/wiki/Go_(programming_language)">Go (programming language) - Wikipedia</a>
    </h2>
    <a class="result__snippet" href="https://en.wikipedia.org/wiki/Go_(programming_language)">Go is a statically typed, compiled programming language designed at <b>Google</b>.</a>
  </div>
</div>
</body></html>`

func TestParseDDGHTMLResults_basic(t *testing.T) {
	results := parseDDGHTMLResults([]byte(ddgHTMLFixture), 10)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d (%+v)", len(results), results)
	}

	// Result 1: Go.dev — DDG redirect URL should be cleaned
	if results[0].title != "The Go Programming Language" {
		t.Errorf("title[0] = %q, want %q", results[0].title, "The Go Programming Language")
	}
	if results[0].link != "https://go.dev/" {
		t.Errorf("link[0] = %q, want %q", results[0].link, "https://go.dev/")
	}
	if results[0].snippet != "Go is an open source programming language that makes it easy to build simple, reliable, and efficient software." {
		t.Errorf("snippet[0] mismatch: %q", results[0].snippet)
	}

	// Result 2: pkg.go.dev — has HTML entities in title (&amp;)
	if results[1].title != "Go standard library & packages" {
		t.Errorf("title[1] = %q, want %q", results[1].title, "Go standard library & packages")
	}
	// Snippet has &#39; which decodes to '
	if results[1].snippet != "Package documentation for Go's standard library." {
		t.Errorf("snippet[1] = %q, want %q", results[1].snippet, "Package documentation for Go's standard library.")
	}

	// Result 3: Wikipedia — direct URL (no redirect), snippet has <b> tag
	if results[2].link != "https://en.wikipedia.org/wiki/Go_(programming_language)" {
		t.Errorf("link[2] = %q", results[2].link)
	}
	if results[2].snippet != "Go is a statically typed, compiled programming language designed at Google." {
		t.Errorf("snippet[2] mismatch: %q", results[2].snippet)
	}
}

func TestParseDDGHTMLResults_maxResults(t *testing.T) {
	results := parseDDGHTMLResults([]byte(ddgHTMLFixture), 1)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].link != "https://go.dev/" {
		t.Errorf("unexpected first result: %+v", results[0])
	}
}

func TestParseDDGHTMLResults_empty(t *testing.T) {
	results := parseDDGHTMLResults([]byte("<html><body>no results here</body></html>"), 10)
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestParseDDGHTMLResults_noTitle(t *testing.T) {
	// If a result link has empty title, the href should be used as title.
	const html = `<div class="result results_links">
  <h2 class="result__title">
    <a class="result__a" href="https://example.com/"></a>
  </h2>
  <a class="result__snippet" href="https://example.com/">A snippet with no title.</a>
</div>`
	results := parseDDGHTMLResults([]byte(html), 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].title != "https://example.com/" {
		t.Errorf("title = %q, want %q", results[0].title, "https://example.com/")
	}
	if results[0].link != "https://example.com/" {
		t.Errorf("link = %q", results[0].link)
	}
}

func TestParseDDGHTMLResults_skipsNonResult(t *testing.T) {
	// Divs without both "result" and "results_links" classes should be skipped.
	const html = `<div class="result">not a real result</div>
<div class="result results_links">
  <h2 class="result__title">
    <a class="result__a" href="https://real.com/">Real</a>
  </h2>
  <a class="result__snippet" href="https://real.com/">Real snippet.</a>
</div>`
	results := parseDDGHTMLResults([]byte(html), 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].link != "https://real.com/" {
		t.Errorf("link = %q", results[0].link)
	}
}

func TestCleanDDGLink(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://example.com", "https://example.com"},
		{"//example.com", "https://example.com"},
		{"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpath", "https://example.com/path"},
		{"  https://example.com  ", "https://example.com"},
	}
	for _, tc := range tests {
		got := cleanDDGLink(tc.input)
		if got != tc.want {
			t.Errorf("cleanDDGLink(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

