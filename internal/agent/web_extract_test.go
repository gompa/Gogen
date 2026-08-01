package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractBySelector(t *testing.T) {
	body := []byte(`<html><body>
		<nav><a href="/a">Home</a></nav>
		<article class="content"><h1>Title</h1><p>Hello <b>world</b></p>
			<pre><code class="language-go">func main() {}</code></pre>
		</article>
		<table><tr><td>a</td><td>b</td></tr></table>
	</body></html>`)

	t.Run("article selector", func(t *testing.T) {
		got, err := extractBySelector(body, "article.content", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "Extracted 1 element(s)") {
			t.Fatalf("missing element count: %q", got)
		}
		if !strings.Contains(got, "# Title") || !strings.Contains(got, "Hello **world**") {
			t.Fatalf("article content missing: %q", got)
		}
		if strings.Contains(got, "Home") {
			t.Fatalf("nav leaked into selector result: %q", got)
		}
	})

	t.Run("code selector", func(t *testing.T) {
		// Select "pre": converting a lone <code> yields inline code; a
		// <pre> block becomes a fenced code block.
		got, err := extractBySelector(body, "pre", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "```go") || !strings.Contains(got, "func main() {}") {
			t.Fatalf("code block not extracted: %q", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		got, err := extractBySelector(body, ".missing", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "no elements match") {
			t.Fatalf("expected no-match message, got %q", got)
		}
	})

	t.Run("invalid selector", func(t *testing.T) {
		if _, err := extractBySelector(body, "div[", ""); err == nil {
			t.Fatal("expected error for invalid selector")
		}
	})

	t.Run("multiple elements", func(t *testing.T) {
		got, err := extractBySelector(body, "td", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "Extracted 2 element(s)") {
			t.Fatalf("expected 2 elements, got %q", got)
		}
		if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
			t.Fatalf("td contents missing: %q", got)
		}
	})
}

func TestSearchExtractedText(t *testing.T) {
	text := "line one\ninstall with go get\nthe install command\nline four\nline five"

	t.Run("match with context", func(t *testing.T) {
		got := searchExtractedText(text, "install", 1)
		if !strings.Contains(got, "2 match(es)") {
			t.Fatalf("expected 2 matches, got %q", got)
		}
		if !strings.Contains(got, "go get") || !strings.Contains(got, "line four") {
			t.Fatalf("match context missing: %q", got)
		}
		if !strings.Contains(got, "[lines") {
			t.Fatalf("expected line ranges: %q", got)
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		got := searchExtractedText(text, "INSTALL", 0)
		if !strings.Contains(got, "2 match(es)") {
			t.Fatalf("case-insensitive search failed: %q", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		got := searchExtractedText(text, "zzz", 1)
		if !strings.Contains(got, "no matches") {
			t.Fatalf("expected no-match message, got %q", got)
		}
	})

	t.Run("merges overlapping windows", func(t *testing.T) {
		// Two matches 2 lines apart with context 3: windows overlap -> merged.
		txt := "a\nb\nMATCH1\nc\nd\nMATCH2\ne"
		got := searchExtractedText(txt, "MATCH", 2)
		if strings.Count(got, "[lines") != 1 {
			t.Fatalf("expected merged window, got %q", got)
		}
	})

	t.Run("caps matches", func(t *testing.T) {
		txt := strings.Repeat("match line\n", 100)
		got := searchExtractedText(txt, "match", 0)
		if !strings.Contains(got, "first 30 shown") {
			t.Fatalf("expected truncation note, got %q", got[:min(200, len(got))])
		}
	})
}

func TestWebFetchSelectorAndQuery(t *testing.T) {
	enableWebFetchForTest(t)
	page := `<html><body>
		<nav><a href="/x">Menu</a></nav>
		<article class="readme"><h1>README</h1><p>Install with <code>go get example</code> today.</p></article>
		<p>unrelated footer text</p>
	</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	exec := NewExecutor(t.TempDir())

	t.Run("selector extraction", func(t *testing.T) {
		out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{Selector: "article.readme"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "# README") || !strings.Contains(out, "go get example") {
			t.Fatalf("selector content missing: %q", out)
		}
		if strings.Contains(out, "Menu") || strings.Contains(out, "unrelated") {
			t.Fatalf("noise leaked into selector result: %q", out)
		}
	})

	t.Run("query search", func(t *testing.T) {
		out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{Query: "go get"})
		if err != nil {
			t.Fatal(err)
		}
		// Match-report format with the match inside its context window; the
		// context legitimately includes neighboring lines.
		if !strings.Contains(out, "1 match(es)") || !strings.Contains(out, "go get example") {
			t.Fatalf("query result missing: %q", out)
		}
		if strings.Contains(out, "Menu") {
			t.Fatalf("nav noise leaked into query result: %q", out)
		}
	})

	t.Run("selector then query", func(t *testing.T) {
		out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{
			Selector: "article.readme",
			Query:    "install",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Install") || !strings.Contains(out, "go get example") {
			t.Fatalf("composed result missing: %q", out)
		}
	})
}
