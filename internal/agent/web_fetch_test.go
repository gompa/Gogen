package agent

import (
	"strings"
	"testing"
)

func TestHTMLToText_basic(t *testing.T) {
	input := []byte(`<html><body><h1>Hello</h1><p>This is a paragraph with <b>bold</b> text.</p></body></html>`)
	got := htmlToText(input)
	want := "Hello\n\nThis is a paragraph with bold text."
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestHTMLToText_stripsScriptStyleHead(t *testing.T) {
	input := []byte(`<html>
<head><title>ignored</title><meta charset="utf-8"></head>
<body>
<style>body { color: red; }</style>
<script>console.log("hi");</script>
<p>Visible text</p>
<noscript>You need JavaScript</noscript>
</body></html>`)
	got := htmlToText(input)
	// <head>, <style>, <script>, <noscript> content should be stripped.
	// <p> introduces a leading newline then "Visible text" then a trailing newline from </p>.
	if !strings.Contains(got, "Visible text") {
		t.Fatalf("missing expected text in: %q", got)
	}
	if strings.Contains(got, "ignored") {
		t.Errorf("head content not stripped: %q", got)
	}
	if strings.Contains(got, "console.log") {
		t.Errorf("script content not stripped: %q", got)
	}
	if strings.Contains(got, "body { color") {
		t.Errorf("style content not stripped: %q", got)
	}
	if strings.Contains(got, "You need JavaScript") {
		t.Errorf("noscript content not stripped: %q", got)
	}
}

func TestHTMLToText_entities(t *testing.T) {
	input := []byte(`<html><body><p>AT&amp;T &lt; Verizon &gt; T-Mobile</p></body></html>`)
	got := htmlToText(input)
	want := "AT&T < Verizon > T-Mobile"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestHTMLToText_blockTags(t *testing.T) {
	// br, hr, and block tags should introduce line breaks.
	input := []byte(`<div>Line 1</div><div>Line 2<br>Line 2.5</div><hr><p>After HR</p>`)
	got := htmlToText(input)
	// Should have line breaks between blocks.
	if !strings.Contains(got, "Line 1") && !strings.Contains(got, "Line 2") && !strings.Contains(got, "After HR") {
		t.Fatalf("unexpected output: %q", got)
	}
	// Verify multiple lines exist.
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines, got %d: %q", len(lines), got)
	}
}

func TestHTMLToText_whitespaceCollapse(t *testing.T) {
	input := []byte("<html><body><p>   lots    of   spaces   </p></body></html>")
	got := htmlToText(input)
	if strings.Contains(got, "   ") {
		t.Fatalf("multiple spaces not collapsed: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("multiple blank lines not collapsed: %q", got)
	}
}

func TestHTMLToText_empty(t *testing.T) {
	got := htmlToText([]byte("<html><head><title>x</title></head><script>y</script><style>z</style><body></body></html>"))
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestHTMLToText_listItems(t *testing.T) {
	// <li> is a block tag, should introduce line breaks.
	input := []byte("<ul><li>First item</li><li>Second item</li></ul>")
	got := htmlToText(input)
	if !strings.Contains(got, "First item") || !strings.Contains(got, "Second item") {
		t.Fatalf("unexpected output: %q", got)
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

func TestHTMLToTextNonHTMLPassthrough(t *testing.T) {
	// Regression: C code with `<` must not be fed to the HTML tokenizer,
	// which would otherwise swallow everything after an unclosed tag
	// (e.g. `a<b` opens a phantom <b> that consumes the rest of the file).
	src := []byte("static const int tbl[] = { 1, 2, 3 };\nint f(int a, int b) { return a<b ? a : b; }\n")
	got := htmlToText(src)
	if !strings.Contains(got, "return a<b ? a : b") {
		t.Fatalf("C source was mangled by HTML parsing: %q", got)
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
