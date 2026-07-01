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
