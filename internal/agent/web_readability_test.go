package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// articlePage is an article-shaped page: nav + sidebar + footer noise around
// a single main <article>. Readability must return just the article.
const articlePage = `<!doctype html>
<html><head><title>Go Blog: The Title</title></head>
<body>
<nav><a href="/menu">Menu item one</a> <a href="/menu2">Menu item two</a></nav>
<div id="sidebar">Related links, ads, junk</div>
<article>
  <h1>The Title</h1>
  <p>The <b>first</b> paragraph of the article body.</p>
  <p>Second paragraph with a <a href="/page2">link</a>.</p>
</article>
<footer>copyright junk</footer>
</body></html>`

func TestExtractReadable(t *testing.T) {
	md, ok := extractReadable([]byte(articlePage), "https://example.com/post")
	if !ok {
		t.Fatal("expected readability to find an article")
	}
	// Readability uses the document <title> as the article title (the same
	// behavior as Firefox reader mode), not the in-body <h1>.
	if !strings.Contains(md, "# Go Blog: The Title") {
		t.Fatalf("missing title: %q", md)
	}
	if !strings.Contains(md, "first") || !strings.Contains(md, "Second paragraph") {
		t.Fatalf("article body missing: %q", md)
	}
	for _, noise := range []string{"Menu item", "sidebar", "copyright"} {
		if strings.Contains(md, noise) {
			t.Fatalf("noise %q leaked into readable extraction: %q", noise, md)
		}
	}
}

func TestExtractReadableNoArticle(t *testing.T) {
	// A client-rendered shell page (no server-side article content) has no
	// single article; readability must fail so the caller falls back to
	// full-page conversion.
	list := `<!doctype html><html><head><title>App</title></head><body>
		<nav><a href="/a">Home</a><a href="/b">About</a></nav>
		<div id="root"></div>
		<script>renderApp()</script>
	</body></html>`
	if _, ok := extractReadable([]byte(list), "https://example.com/"); ok {
		t.Fatal("expected no readable content for a link-list page")
	}
}

func TestWebFetchReadabilityDefault(t *testing.T) {
	enableWebFetchForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articlePage))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	exec := NewExecutor(t.TempDir())
	out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Go Blog: The Title") || !strings.Contains(out, "Second paragraph") {
		t.Fatalf("condensed article missing: %q", out)
	}
	for _, noise := range []string{"Menu item", "sidebar", "copyright junk"} {
		if strings.Contains(out, noise) {
			t.Fatalf("noise %q leaked into default fetch: %q", noise, out)
		}
	}
}

func TestWebFetchReadabilityFallback(t *testing.T) {
	enableWebFetchForTest(t)
	// JS-shell page: readability finds nothing, so the full-page path runs
	// (and yields "(empty body)" since nav/script are stripped).
	shell := `<!doctype html><html><head><title>App</title></head><body>
		<nav><a href="/a">Home</a><a href="/b">About</a></nav>
		<div id="root"></div>
		<script>renderApp()</script>
	</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(shell))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	exec := NewExecutor(t.TempDir())
	out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no single main article") {
		t.Fatalf("expected fallback note, got: %q", out)
	}
	if !strings.Contains(out, "(empty body)") {
		t.Fatalf("expected empty-body marker after fallback, got: %q", out)
	}
}

// TestWebFetchSelectorBypassesReadability pins that an explicit selector
// still takes precedence over the readability default.
func TestWebFetchSelectorBypassesReadability(t *testing.T) {
	enableWebFetchForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articlePage))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	exec := NewExecutor(t.TempDir())
	out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{Selector: "article"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Extracted 1 element(s)") {
		t.Fatalf("selector path not taken: %q", out)
	}
}
