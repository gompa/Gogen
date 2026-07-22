package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	webSearchTimeout    = 15 * time.Second
	webSearchMaxResults = 10
	webSearchMaxOutput  = 8 * 1024 // max bytes in the tool result

	// ddgUA is the User-Agent sent to DuckDuckGo. A browser-like UA avoids
	// triggering aggressive bot detection on the html.duckduckgo.com endpoint.
	ddgUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
)

// WebSearch runs a search query using DuckDuckGo HTML by default, or Brave API
// when GOGEN_WEB_SEARCH_API_KEY and GOGEN_WEB_SEARCH_BACKEND=brave are set.
func (e *Executor) WebSearch(ctx context.Context, query string, maxResults int) (string, error) {
	if !webCfg.isSearchOn() {
		return "", fmt.Errorf("web_search is disabled (set GOGEN_WEB_SEARCH=on to re-enable)")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	if maxResults <= 0 {
		maxResults = webSearchMaxResults
	}
	if maxResults > 20 {
		maxResults = 20
	}

	if webCfg.searchBE() == "brave" {
		if apiKey := webCfg.searchKey(); apiKey != "" {
			return searchBrave(ctx, query, maxResults, apiKey)
		}
	}
	return searchDuckDuckGoHTML(ctx, query, maxResults)
}

// searchDuckDuckGoHTML uses the html.duckduckgo.com no-JS endpoint.
// Results are returned as structured HTML with .result__title and
// .result__snippet classes.
func searchDuckDuckGoHTML(ctx context.Context, query string, maxResults int) (string, error) {
	// Build URL via url.Values for proper encoding, then attach to the base.
	u, err := url.Parse("https://html.duckduckgo.com/html/")
	if err != nil {
		return "", fmt.Errorf("search: %w", err)
	}
	u.RawQuery = url.Values{"q": {query}}.Encode()

	body, _, err := doFetch(ctx, fetchRequest{
		URL:      u.String(),
		UA:       ddgUA,
		MaxBytes: 512 * 1024,
	})
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	results := parseDDGHTMLResults(body, maxResults)
	if len(results) == 0 {
		return fmt.Sprintf("No results found for %q (via DuckDuckGo)", query), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q (via DuckDuckGo):\n\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n", i+1, r.title)
		fmt.Fprintf(&b, "   URL: %s\n", r.link)
		if r.snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.snippet)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Use web_fetch to retrieve full content from result URLs.")
	return b.String(), nil
}

type searchResult struct {
	title   string
	link    string
	snippet string
}

// parseDDGHTMLResults extracts search results from the html.duckduckgo.com
// response. The DOM uses .result div wrappers containing an .result__title
// heading (with .result__a inside for the link) and a .result__snippet anchor.
//
// Example structure:
//
//	<div class="result results_links results_links_deep web-result ">
//	  <h2 class="result__title">
//	    <a class="result__a" href="//duckduckgo.com/l/?uddg=...">Title text</a>
//	  </h2>
//	  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=...">Snippet <b>with</b> bold</a>
//	</div>
func parseDDGHTMLResults(body []byte, maxResults int) []searchResult {
	z := html.NewTokenizer(bytes.NewReader(body))
	var results []searchResult

	var (
		inResult    bool
		resultDepth int // >0 inside result; emit when drops to 0
		inTitle     bool
		inSnippet   bool
		titleTag    string // tag name that opened result__title (e.g. "h2")
		snippetTag  string // tag name that opened result__snippet (e.g. "a")
		href        string
		titleBuf    strings.Builder
		snipBuf     strings.Builder
	)

	for {
		if len(results) >= maxResults {
			break
		}
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		tok := z.Token()

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			// <div class="result results_links ...">  → enter result
			if tok.Data == "div" && tokenHasClass(tok, "result") && tokenHasClass(tok, "results_links") {
				inResult = true
				resultDepth = 0
				href = ""
				titleBuf.Reset()
				snipBuf.Reset()
			} else if inResult && tok.Data == "div" {
				resultDepth++
			}
			if inResult {
				// <a class="result__a" href="...">  → capture link (inside title h2)
				if tok.Data == "a" && tokenHasClass(tok, "result__a") {
					for _, a := range tok.Attr {
						if a.Key == "href" {
							href = a.Val
							break
						}
					}
				}
				// Any element with class="result__title"  → start collecting title text
				if tokenHasClass(tok, "result__title") {
					inTitle = true
					titleTag = tok.Data
				}
				// Any element with class="result__snippet"  → start collecting snippet text
				if tokenHasClass(tok, "result__snippet") {
					inSnippet = true
					snippetTag = tok.Data
				}
			}

		case html.EndTagToken:
			// Close title/snippet when the matching element ends.
			if inTitle && tok.Data == titleTag {
				inTitle = false
			}
			if inSnippet && tok.Data == snippetTag {
				inSnippet = false
			}
			// Track div nesting; emit result when outer wrapper closes.
			if inResult && tok.Data == "div" {
				resultDepth--
				if resultDepth < 0 {
					inResult = false
					title := html.UnescapeString(strings.TrimSpace(titleBuf.String()))
					snippet := html.UnescapeString(strings.TrimSpace(snipBuf.String()))
					link := cleanDDGLink(href)
					if title == "" && link == "" {
						continue
					}
					if title == "" {
						title = link
					}
					results = append(results, searchResult{title: title, link: link, snippet: snippet})
				}
			}

		case html.TextToken:
			if inTitle {
				titleBuf.WriteString(string(tok.Data))
			} else if inSnippet {
				snipBuf.WriteString(string(tok.Data))
			}
		}
	}
	return results
}

// tokenHasClass reports whether an HTML token's class attribute contains
// the given CSS class as a whitespace-separated token.
func tokenHasClass(tok html.Token, class string) bool {
	for _, a := range tok.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if strings.EqualFold(c, class) {
					return true
				}
			}
			return false
		}
	}
	return false
}

func cleanDDGLink(raw string) string {
	raw = strings.TrimSpace(raw)
	// DuckDuckGo sometimes wraps links in a redirect.
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	if strings.Contains(raw, "duckduckgo.com/l/?uddg=") {
		parsed, err := url.Parse(raw)
		if err == nil {
			if dest := parsed.Query().Get("uddg"); dest != "" {
				decoded, err := url.PathUnescape(dest)
				if err == nil {
					return decoded
				}
				return dest
			}
		}
	}
	return raw
}

// searchBrave uses the Brave Web Search API (requires GOGEN_WEB_SEARCH_API_KEY).
func searchBrave(ctx context.Context, query string, maxResults int, apiKey string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()
	client := &http.Client{}
	reqURL := "https://api.search.brave.com/res/v1/web/search?" + url.Values{
		"q":     {query},
		"count": {strconv.Itoa(maxResults)},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("brave request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("brave search: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", fmt.Errorf("brave read: %w", err)
	}
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", fmt.Errorf("brave api %d: %s", resp.StatusCode, msg)
	}

	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("brave parse: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q (via Brave):\n\n", query)
	for i, r := range parsed.Web.Results {
		if i >= maxResults {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, r.Title)
		fmt.Fprintf(&b, "   URL: %s\n", r.URL)
		if r.Description != "" {
			fmt.Fprintf(&b, "   %s\n", r.Description)
		}
		b.WriteByte('\n')
	}
	if len(parsed.Web.Results) == 0 {
		b.WriteString("No results found.\n")
	}
	fmt.Fprintf(&b, "Use web_fetch to retrieve full content from result URLs.")
	return b.String(), nil
}
