package agent

import (
	"bytes"
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

func htmlToText(body []byte) string {
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
