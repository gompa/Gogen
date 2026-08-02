package agent

import (
	"bytes"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"codeberg.org/readeck/go-readability/v2"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
)

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

// mdConverter converts HTML documents to readable Markdown for the browsing
// use case. It is safe for concurrent use (the library guards internally).
//
// Configuration notes, empirically driven (see web_fetch_test.go):
//   - nav/footer/aside are removed: on content-heavy sites like GitHub these
//     containers dominate the byte budget with menus before the real content
//     appears (a repo page fetched at 128 KB yielded 1.8 KB of pure
//     navigation and zero README).
//   - img is removed: image markup is token noise for the model and the alt
//     text rarely carries information worth the URL.
//   - Links with an empty destination or empty content render as plain text
//     instead of stray "[]()" artifacts (e.g. image-only links).
//   - List-end comments are disabled: the commonmark plugin otherwise
//     inserts "<!--THE END-->" markers between adjacent lists (a round-trip
//     fidelity aid) that leak into the output (MDN showed this).
//   - form/dialog/template/button are removed: they carry interactive chrome
//     (search forms, overlays, JS templates, action buttons) that never
//     contains article content. Verified against pkg.go.dev and GitHub:
//     the noise disappears while the content survives.
//   - header is deliberately KEPT: page titles commonly live in <h1> inside
//     <header> (e.g. pkg.go.dev's package title) and are worth more than the
//     site-header links it also carries.
//
// The base plugin already removes head/script/style/noscript/iframe/input/
// textarea; the commonmark plugin emits headings, fenced code blocks (with
// indentation intact), lists, blockquotes, and inline links.
var mdConverter = func() *converter.Converter {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(
			commonmark.WithLinkEmptyContentBehavior(commonmark.LinkBehaviorSkip),
			commonmark.WithLinkEmptyHrefBehavior(commonmark.LinkBehaviorSkip),
			commonmark.WithListEndComment(false),
		),
	))
	conv.Register.TagType("nav", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("footer", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("aside", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("img", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("form", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("dialog", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("template", converter.TagTypeRemove, converter.PriorityStandard)
	conv.Register.TagType("button", converter.TagTypeRemove, converter.PriorityStandard)
	return conv
}()

// htmlToMarkdown converts an HTML document into readable Markdown, making
// relative links absolute against baseURL (may be empty). On conversion
// failure the body is returned as plain text rather than erroring out.
func htmlToMarkdown(body []byte, baseURL string) string {
	var opts []converter.ConvertOptionFunc
	if baseURL != "" {
		opts = append(opts, converter.WithDomain(baseURL))
	}
	md, err := mdConverter.ConvertString(string(body), opts...)
	if err != nil {
		return plainText(body)
	}
	return strings.TrimSpace(md)
}

// extractReadable attempts main-content extraction on an HTML body using the
// Readability algorithm (the same one Firefox reader mode uses). When the page
// contains a single main article it returns the condensed result — the page
// title as a heading plus the article converted to Markdown — and ok=true.
// Otherwise ok=false so the caller can fall back to full-page conversion.
//
// This is the "best condensed information" default: unlike the tag-based
// chrome removal in htmlToMarkdown, Readability scores nodes by text and link
// density, so boilerplate survives no matter what tag it lives in (a sidebar
// of links is pruned whether it is <nav> or a bare <div> portlet).
func extractReadable(body []byte, baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		u = &url.URL{}
	}
	article, err := readability.FromReader(bytes.NewReader(body), u)
	if err != nil || article.Node == nil {
		return "", false
	}
	var htmlBuf bytes.Buffer
	if err := article.RenderHTML(&htmlBuf); err != nil {
		return "", false
	}
	md := htmlToMarkdown(htmlBuf.Bytes(), baseURL)
	if strings.TrimSpace(md) == "" {
		return "", false
	}
	var b strings.Builder
	if title := strings.TrimSpace(article.Title()); title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	b.WriteString(md)
	return b.String(), true
}

// isHTMLLike reports whether extractResponseText would treat the response as
// HTML (in which case readability extraction is attempted before falling back
// to full-page conversion). It mirrors the HTML branches of extractResponseText.
func isHTMLLike(contentType, finalURL string, body []byte) bool {
	if isHTMLContentType(contentType) {
		return true
	}
	if isPlainContentType(contentType) {
		return false
	}
	if isHTMLURL(finalURL) {
		return true
	}
	if isPlainURL(finalURL) {
		return false
	}
	return looksLikeHTML(body)
}

// extractResponseText converts a fetched response body into text. HTML
// responses are converted to Markdown via htmlToMarkdown; everything else
// (source files, JSON, plain text, ...) is returned as-is so `<` characters
// and code formatting survive.
//
// Content-Type wins when it is authoritative; for ambiguous or missing types
// the URL extension and a body sniff decide.
func extractResponseText(contentType, finalURL string, body []byte) string {
	switch {
	case isHTMLContentType(contentType):
		return htmlToMarkdown(body, finalURL)
	case isPlainContentType(contentType):
		return plainText(body)
	case isHTMLURL(finalURL):
		return htmlToMarkdown(body, finalURL)
	case isPlainURL(finalURL):
		return plainText(body)
	case looksLikeHTML(body):
		return htmlToMarkdown(body, finalURL)
	default:
		return plainText(body)
	}
}
