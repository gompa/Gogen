package agent

import (
	"strings"
	"testing"
)

func TestNormalizeFetchMode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to https", "", "https"},
		{"https kept", "https", "https"},
		{"https case/space normalized", "  HTTPS ", "https"},
		{"all kept", "all", "all"},
		{"typo fails closed to https", "hhtps", "https"},
		{"http fails closed to https", "http", "https"},
		{"garbage fails closed to https", "mixed", "https"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeFetchMode(tt.in); got != tt.want {
				t.Errorf("normalizeFetchMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHTMLToMarkdown_basic(t *testing.T) {
	input := []byte(`<html><body><h1>Hello</h1><p>This is a paragraph with <b>bold</b> text.</p></body></html>`)
	got := htmlToMarkdown(input, "")
	want := "# Hello\n\nThis is a paragraph with **bold** text."
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestHTMLToMarkdown_stripsNoise(t *testing.T) {
	input := []byte(`<html>
<head><title>ignored</title><meta charset="utf-8"></head>
<body>
<style>body { color: red; }</style>
<script>console.log("hi");</script>
<p>Visible text</p>
<noscript>You need JavaScript</noscript>
</body></html>`)
	got := htmlToMarkdown(input, "")
	want := "Visible text"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHTMLToMarkdown_entities(t *testing.T) {
	input := []byte(`<html><body><p>AT&amp;T &lt; Verizon &gt; T-Mobile</p></body></html>`)
	got := htmlToMarkdown(input, "")
	// The library deliberately keeps < and > entity-escaped in text (its
	// safety choice for markdown); &amp; is unescaped. Cosmetic, but pinned
	// here so the behavior is deliberate rather than accidental.
	want := "AT&T &lt; Verizon &gt; T-Mobile"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestHTMLToMarkdown_blockBreaks(t *testing.T) {
	// br, hr, and block tags should introduce line breaks.
	input := []byte(`<div>Line 1</div><div>Line 2<br>Line 2.5</div><hr><p>After HR</p>`)
	got := htmlToMarkdown(input, "")
	if !strings.Contains(got, "Line 1") && !strings.Contains(got, "Line 2") && !strings.Contains(got, "After HR") {
		t.Fatalf("unexpected output: %q", got)
	}
	if !strings.Contains(got, "* * *") {
		t.Fatalf("hr should become a thematic break: %q", got)
	}
	// Verify multiple lines exist.
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d: %q", len(lines), got)
	}
}

func TestHTMLToMarkdown_whitespaceCollapse(t *testing.T) {
	input := []byte("<html><body><p>   lots    of   spaces   </p></body></html>")
	got := htmlToMarkdown(input, "")
	if got != "lots of spaces" {
		t.Fatalf("got %q, want %q", got, "lots of spaces")
	}
}

func TestHTMLToMarkdown_empty(t *testing.T) {
	got := htmlToMarkdown([]byte("<html><head><title>x</title></head><script>y</script><style>z</style><body></body></html>"), "")
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestHTMLToMarkdown_listItems(t *testing.T) {
	input := []byte("<ul><li>First item</li><li>Second item</li></ul>")
	got := htmlToMarkdown(input, "")
	want := "- First item\n- Second item"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHTMLToMarkdown_codeBlocksPreserved(t *testing.T) {
	// Regression: the old hand-rolled tokenizer collapsed 3+ spaces,
	// destroying code indentation. Fenced code blocks must keep it intact.
	input := []byte("<pre><code class=\"language-go\">func main() {\n\t\tfmt.Println(\"hi\")\n\t}</code></pre>")
	got := htmlToMarkdown(input, "")
	want := "```go\nfunc main() {\n\t\tfmt.Println(\"hi\")\n\t}\n```"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestHTMLToMarkdown_boilerplateRemoved(t *testing.T) {
	// Empirical: nav/footer/aside dominate pages like GitHub and starve the
	// byte budget before the real content appears.
	// Interactive chrome (forms, dialogs, JS templates, buttons) is removed
	// for the same reason: it is never article content.
	input := []byte(`<nav><a href="/a">Home</a><a href="/b">Pricing</a></nav><footer>Copyright 2026</footer><aside>Sidebar ad</aside><form><input name="q" placeholder="Search or jump to..."></form><dialog>Provide feedback</dialog><template>{{ message }}</template><button type="button">Open menu</button><main><p>Real content</p></main>`)
	got := htmlToMarkdown(input, "")
	if got != "Real content" {
		t.Fatalf("got %q, want %q", got, "Real content")
	}
}

func TestHTMLToMarkdown_headerKept(t *testing.T) {
	// Page titles commonly live in <h1> inside <header> (pkg.go.dev's
	// package title is one example); removing header would lose them.
	input := []byte(`<header><h1>Package htmltomarkdown</h1></header><main><p>Body</p></main>`)
	got := htmlToMarkdown(input, "")
	want := "# Package htmltomarkdown\n\nBody"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHTMLToMarkdown_noListEndComments(t *testing.T) {
	// Regression: the commonmark plugin inserts "<!--THE END-->" markers
	// between adjacent lists (a round-trip fidelity aid). They must not
	// leak into the output — MDN's a11y-menu <ul> followed by the
	// breadcrumb <ol> exposed exactly this.
	input := []byte(`<ul><li>Skip</li></ul><ol><li>Web</li></ol>`)
	got := htmlToMarkdown(input, "")
	if strings.Contains(got, "THE END") || strings.Contains(got, "<!--") {
		t.Fatalf("list-end comment leaked into output: %q", got)
	}
	if !strings.Contains(got, "- Skip") || !strings.Contains(got, "1. Web") {
		t.Fatalf("adjacent lists not rendered: %q", got)
	}
}

func TestHTMLToMarkdown_linksAndImages(t *testing.T) {
	cases := []struct {
		name    string
		html    string
		baseURL string
		want    string
	}{
		{
			name:    "links rendered",
			html:    `<p>See <a href="https://example.com/doc">the docs</a>.</p>`,
			baseURL: "",
			want:    "See [the docs](https://example.com/doc).",
		},
		{
			name:    "relative links made absolute",
			html:    `<p>See <a href="/rel">this</a>.</p>`,
			baseURL: "https://example.com",
			want:    "See [this](https://example.com/rel).",
		},
		{
			name:    "images removed",
			html:    `<p><a href="https://example.com/"><img src="https://example.com/b.png" alt="build"></a> done</p>`,
			baseURL: "",
			want:    "done",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToMarkdown([]byte(tc.html), tc.baseURL); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"doctype", []byte("<!DOCTYPE html>\n<html><body>hi</body></html>"), true},
		{"html tag", []byte("<html><head><title>x</title></head></html>"), true},
		{"div body", []byte("<div class=\"x\">hello</div>"), true},
		{"c source", []byte("#include <stdio.h>\nint main(void) { return a<b ? 1 : 0; }\n"), false},
		{"plain text", []byte("just some text\nwith a < b comparison\n"), false},
		{"json", []byte(`{"a": 1, "b": "<not-html>"}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeHTML(tc.body); got != tc.want {
				t.Fatalf("looksLikeHTML(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestPlainTextPreservesFormatting(t *testing.T) {
	body := []byte("static const int tbl[] = {\r\n\t{ 0x01, \"a\", 1 << 0 },\r\n};\r\n")
	got := plainText(body)
	want := "static const int tbl[] = {\n\t{ 0x01, \"a\", 1 << 0 },\n};"
	if got != want {
		t.Fatalf("plainText = %q, want %q", got, want)
	}
}

func TestPlainTextBinaryGuard(t *testing.T) {
	got := plainText([]byte("abc\x00def"))
	if !strings.Contains(got, "binary content") {
		t.Fatalf("expected binary notice, got %q", got)
	}
}

func TestExtractResponseText(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		finalURL    string
		body        []byte
		want        string
	}{
		{
			name:        "html content type strips tags",
			contentType: "text/html; charset=utf-8",
			finalURL:    "https://example.com/",
			body:        []byte("<html><body><p>Hello</p></body></html>"),
			want:        "Hello",
		},
		{
			name:        "plain content type returns raw text",
			contentType: "text/plain; charset=utf-8",
			finalURL:    "https://example.com/f.c",
			body:        []byte("#include <stdio.h>\nint a<b;\n"),
			want:        "#include <stdio.h>\nint a<b;",
		},
		{
			name:        "text/plain wins over html-looking body",
			contentType: "text/plain",
			finalURL:    "https://example.com/gen.go",
			body:        []byte("// renders a <div> for the user\n"),
			want:        "// renders a <div> for the user",
		},
		{
			name:        "octet-stream with source extension returns raw text",
			contentType: "application/octet-stream",
			finalURL:    "https://cdn.example.com/smu.c",
			body:        []byte("int x = a<b;\n"),
			want:        "int x = a<b;",
		},
		{
			name:        "html content type wins over body sniff",
			contentType: "text/html",
			finalURL:    "https://example.com/raw.c",
			body:        []byte("<html><body>t</body></html>"),
			want:        "t",
		},
		{
			name:        "missing type html body sniffed",
			contentType: "",
			finalURL:    "https://example.com/page",
			body:        []byte("<html><body>hi</body></html>"),
			want:        "hi",
		},
		{
			name:        "missing type plain body returned as-is",
			contentType: "",
			finalURL:    "https://example.com/blob",
			body:        []byte("line1\nline2\n"),
			want:        "line1\nline2",
		},
		{
			name:        "html url wins over sniff",
			contentType: "application/octet-stream",
			finalURL:    "https://example.com/page.html",
			body:        []byte("<html><body>t</body></html>"),
			want:        "t",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractResponseText(tc.contentType, tc.finalURL, tc.body); got != tc.want {
				t.Fatalf("extractResponseText(%q, %q, %d bytes) = %q, want %q",
					tc.contentType, tc.finalURL, len(tc.body), got, tc.want)
			}
		})
	}
}

// TestClassifyResponse pins the HTML/plain classification — the single source
// of truth shared by extractResponseText and extractWebContent. Content-Type
// wins when it is authoritative; otherwise the URL extension and a body sniff
// decide.
func TestClassifyResponse(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		finalURL    string
		body        []byte
		want        responseKind
	}{
		{"html content type", "text/html; charset=utf-8", "https://example.com/", []byte("<html><body><p>Hello</p></body></html>"), kindHTML},
		{"plain content type", "text/plain; charset=utf-8", "https://example.com/f.c", []byte("#include <stdio.h>\nint a<b;\n"), kindPlain},
		{"text/plain wins over html-looking body", "text/plain", "https://example.com/gen.go", []byte("// renders a <div> for the user\n"), kindPlain},
		{"octet-stream with source extension", "application/octet-stream", "https://cdn.example.com/smu.c", []byte("int x = a<b;\n"), kindPlain},
		{"html content type wins over body sniff", "text/html", "https://example.com/raw.c", []byte("<html><body>t</body></html>"), kindHTML},
		{"missing type html body sniffed", "", "https://example.com/page", []byte("<html><body>hi</body></html>"), kindHTML},
		{"missing type plain body", "", "https://example.com/blob", []byte("line1\nline2\n"), kindPlain},
		{"html url wins over sniff", "application/octet-stream", "https://example.com/page.html", []byte("<html><body>t</body></html>"), kindHTML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyResponse(tc.contentType, tc.finalURL, tc.body); got != tc.want {
				t.Fatalf("classifyResponse(%q, %q, %d bytes) = %v, want %v",
					tc.contentType, tc.finalURL, len(tc.body), got, tc.want)
			}
		})
	}
}
