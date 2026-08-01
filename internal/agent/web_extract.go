package agent

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/ericchiang/css"
	"golang.org/x/net/html"
)

const (
	// webFetchExtractMaxElements caps how many selector matches are converted per
	// call, to keep the tool result bounded.
	webFetchExtractMaxElements = 50
	// webFetchSearchMaxMatches caps how many query matches are returned.
	webFetchSearchMaxMatches = 30
	// webFetchSearchDefaultContext is the context window around each match when the
	// caller does not specify one.
	webFetchSearchDefaultContext = 3
	// webFetchSearchMaxContextLines bounds the per-match context window.
	webFetchSearchMaxContextLines = 10
)

// extractBySelector parses body as HTML, selects the elements matching a CSS
// selector, and converts each to markdown. This is the generic escape hatch
// for unseen pages: instead of pre-tuning boilerplate removal, the caller
// names exactly what it wants (e.g. "article", ".markdown-body", "table",
// "pre"). Elements that fail conversion are skipped; a selector with no
// matches returns an explicit message rather than an error.
func extractBySelector(body []byte, selector, baseURL string) (string, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}
	sel, err := css.Parse(selector)
	if err != nil {
		return "", fmt.Errorf("invalid selector %q: %w", selector, err)
	}
	nodes := sel.Select(doc)
	if len(nodes) == 0 {
		return fmt.Sprintf("no elements match selector %q", selector), nil
	}

	var opts []converter.ConvertOptionFunc
	if baseURL != "" {
		opts = append(opts, converter.WithDomain(baseURL))
	}

	var b strings.Builder
	if len(nodes) > webFetchExtractMaxElements {
		fmt.Fprintf(&b, "Extracted first %d of %d elements matching %q:\n\n", webFetchExtractMaxElements, len(nodes), selector)
		nodes = nodes[:webFetchExtractMaxElements]
	} else {
		fmt.Fprintf(&b, "Extracted %d element(s) matching %q:\n\n", len(nodes), selector)
	}
	for i, n := range nodes {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		md, err := mdConverter.ConvertNode(n, opts...)
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(md)); s != "" {
			b.WriteString(s)
		}
	}
	return b.String(), nil
}

// searchExtractedText finds case-insensitive literal matches of query in text
// and returns each match with surrounding context lines, merging overlapping
// windows. This is the "search within a fetched page" primitive: the caller
// gets the relevant slices of a large or noisy page instead of the whole
// thing.
func searchExtractedText(text, query string, contextLines int) string {
	lines := strings.Split(text, "\n")
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > webFetchSearchMaxContextLines {
		contextLines = webFetchSearchMaxContextLines
	}
	if contextLines == 0 {
		contextLines = webFetchSearchDefaultContext
	}

	low := strings.ToLower(query)
	var matches []int
	for i, l := range lines {
		if strings.Contains(strings.ToLower(l), low) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q", query)
	}

	shown := matches
	truncated := len(matches) > webFetchSearchMaxMatches
	if truncated {
		shown = matches[:webFetchSearchMaxMatches]
	}

	// Build per-match context windows, merging overlaps.
	type win struct{ start, end int }
	var wins []win
	for _, lineNo := range shown {
		start := lineNo - contextLines
		if start < 0 {
			start = 0
		}
		end := lineNo + contextLines + 1
		if end > len(lines) {
			end = len(lines)
		}
		if n := len(wins); n > 0 && start <= wins[n-1].end {
			if end > wins[n-1].end {
				wins[n-1].end = end
			}
		} else {
			wins = append(wins, win{start, end})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q", len(matches), query)
	if truncated {
		fmt.Fprintf(&b, " (first %d shown)", len(shown))
	}
	b.WriteString(":\n")
	for _, w := range wins {
		fmt.Fprintf(&b, "\n[lines %d-%d]\n", w.start+1, w.end)
		b.WriteString(strings.Join(lines[w.start:w.end], "\n"))
		b.WriteString("\n")
	}
	return b.String()
}
