package agent

import (
	"bytes"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var (
	multiSpaceRE   = regexp.MustCompile(`\s{3,}`)
	multiNewlineRE = regexp.MustCompile(`\n{3,}`)
)

// blockTags are HTML elements that introduce paragraph-like breaks.
var blockTags = map[string]bool{
	"br": true, "p": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"div": true, "section": true, "article": true, "header": true, "footer": true,
	"nav": true, "aside": true, "main": true, "figure": true, "figcaption": true,
	"blockquote": true, "pre": true, "table": true, "ul": true, "ol": true,
	"dl": true, "dt": true, "dd": true, "form": true, "fieldset": true,
}

// plainFileExts are file extensions whose contents are textual source or
// data files. web_fetch serves these as-is instead of parsing them as HTML,
// so `<` characters and code formatting survive.
var plainFileExts = map[string]bool{
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true, ".hpp": true, ".hh": true,
	".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".ts": true,
	".tsx": true, ".rs": true, ".java": true, ".kt": true, ".kts": true, ".swift": true, ".rb": true,
	".php": true, ".pl": true, ".lua": true, ".sh": true, ".bash": true, ".zsh": true, ".fish": true,
	".md": true, ".markdown": true, ".txt": true, ".text": true, ".json": true, ".yaml": true,
	".yml": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true, ".log": true, ".csv": true,
	".tsv": true, ".xml": true, ".svg": true, ".css": true, ".scss": true, ".less": true, ".sql": true,
	".diff": true, ".patch": true, ".proto": true, ".cmake": true, ".mk": true, ".rst": true, ".tex": true,
	".gitignore": true, ".gitattributes": true, ".env": true, ".editorconfig": true,
}

// htmlMarkers are substrings that indicate a document is HTML rather than
// plain text or source code. They are used as a fallback when the
// Content-Type header is missing or ambiguous (e.g. application/octet-stream).
var htmlMarkers = []string{
	"<!doctype html", "<html", "<head", "<body", "<title", "<meta",
	"<div", "<span", "<p>", "<p ", "<a href", "<h1", "<h2", "<h3",
	"<h4", "<h5", "<h6", "<table", "<ul", "<ol", "<li", "<script",
	"<style", "<form", "<img", "<link", "<nav", "<header", "<footer",
	"<article", "<section", "<pre", "<blockquote", "<input", "<iframe", "<svg",
}

// looksLikeHTML reports whether body appears to be an HTML document rather
// than plain text or source code. Only the beginning of the body is scanned.
func looksLikeHTML(body []byte) bool {
	prefix := body
	if len(prefix) > 2048 {
		prefix = prefix[:2048]
	}
	low := strings.ToLower(string(prefix))
	for _, m := range htmlMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// plainText returns the body as text with formatting (indentation, line
// structure) preserved. Binary payloads are reported instead of dumped.
func plainText(body []byte) string {
	if bytes.IndexByte(body, 0) >= 0 {
		return fmt.Sprintf("(binary content: %d bytes, not shown)", len(body))
	}
	return strings.TrimSpace(strings.ReplaceAll(string(body), "\r\n", "\n"))
}

// contentTypeBase returns the media type with parameters stripped and
// lowercased (e.g. "text/plain; charset=utf-8" -> "text/plain").
func contentTypeBase(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// isHTMLContentType reports whether a Content-Type header denotes HTML.
func isHTMLContentType(ct string) bool {
	switch contentTypeBase(ct) {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}

// isPlainContentType reports whether a Content-Type header denotes plain
// text (as opposed to HTML or binary data).
func isPlainContentType(ct string) bool {
	base := contentTypeBase(ct)
	if base == "" {
		return false
	}
	if strings.HasPrefix(base, "text/") && base != "text/html" {
		return true
	}
	switch base {
	case "application/json", "application/javascript", "application/x-javascript",
		"application/ecmascript", "application/xml", "application/graphql":
		return true
	}
	return false
}

// isHTMLURL reports whether a URL path looks like an HTML page.
func isHTMLURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".htm") || strings.HasSuffix(p, ".xhtml")
}

// isPlainURL reports whether a URL path names a known plain-text/source file.
func isPlainURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return plainFileExts[strings.ToLower(filepath.Ext(u.Path))]
}

// extractResponseText converts a fetched response body into text. HTML
// responses are stripped of markup via htmlToText; everything else (source
// files, JSON, plain text, ...) is returned as-is so `<` characters and code
// formatting survive.
//
// Content-Type wins when it is authoritative; for ambiguous or missing types
// the URL extension and a body sniff decide.
func extractResponseText(contentType, finalURL string, body []byte) string {
	switch {
	case isHTMLContentType(contentType):
		return htmlToText(body)
	case isPlainContentType(contentType):
		return plainText(body)
	case isHTMLURL(finalURL):
		return htmlToText(body)
	case isPlainURL(finalURL):
		return plainText(body)
	case looksLikeHTML(body):
		return htmlToText(body)
	default:
		return plainText(body)
	}
}

func htmlToText(body []byte) string {
	// Only tokenize as HTML when the content actually looks like HTML.
	// Source files (e.g. C code) contain plenty of `<` characters that the
	// tokenizer would otherwise interpret as (possibly unclosed) tags,
	// silently dropping everything after the phantom tag.
	if !looksLikeHTML(body) {
		return plainText(body)
	}
	z := html.NewTokenizer(bytes.NewReader(body))
	var b strings.Builder

	var (
		skipDepth int    // > 0 when inside a <script>, <style>, <head>, or <noscript>
		skipTag   string // tag name that opened the skip region
	)

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		tok := z.Token()

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			if skipDepth > 0 {
				if tok.Data == skipTag {
					skipDepth++
				}
				continue
			}
			switch tok.Data {
			case "script", "style", "head", "noscript":
				skipTag = tok.Data
				skipDepth = 1
			case "br", "hr":
				b.WriteByte('\n')
			default:
				if blockTags[tok.Data] {
					b.WriteByte('\n')
				}
			}

		case html.EndTagToken:
			if skipDepth > 0 {
				if tok.Data == skipTag {
					skipDepth--
				}
				continue
			}
			if blockTags[tok.Data] {
				b.WriteByte('\n')
			}

		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			b.WriteString(tok.Data)
		}
	}

	text := html.UnescapeString(b.String())
	// Collapse whitespace.
	text = multiNewlineRE.ReplaceAllString(text, "\n\n")
	text = multiSpaceRE.ReplaceAllString(text, "  ")
	return strings.TrimSpace(text)
}
