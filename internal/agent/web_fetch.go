package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	webFetchTimeout      = 15 * time.Second
	webFetchDefaultMax   = 64 * 1024 // 64 KB — plenty for docs pages
	webFetchHardMax      = 2 * 1024 * 1024
	webFetchMaxRedirects = 3
	webFetchOutputLimit  = 64 * 1024 // max bytes returned in the tool result string
)

var (
	fetchPrivateRE = regexp.MustCompile(`^127\.|^10\.|^172\.(1[6-9]|2[0-9]|3[01])\.|^192\.168\.|^0\.0\.0\.0$|^::1$|^fe80:`)
	multiSpaceRE   = regexp.MustCompile(`\s{3,}`)
	multiNewlineRE = regexp.MustCompile(`\n{3,}`)

	webMu      sync.RWMutex
	webEnabled *bool // nil until configured
	webMode    string // "https" or "all"
	webDomains []string
)

// blockTags are HTML elements that introduce paragraph-like breaks.
// This list mirrors the inline regex that was previously used in htmlToText.
var blockTags = map[string]bool{
	"br": true, "p": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"div": true, "section": true, "article": true, "header": true, "footer": true,
	"nav": true, "aside": true, "main": true, "figure": true, "figcaption": true,
	"blockquote": true, "pre": true, "table": true, "ul": true, "ol": true,
	"dl": true, "dt": true, "dd": true, "form": true, "fieldset": true,
}

// ConfigureWebFetch applies runtime web fetch settings from merged config.
func ConfigureWebFetch(enabled bool, mode string, allowedDomains string) {
	webMu.Lock()
	defer webMu.Unlock()
	webEnabled = &enabled
	webMode = strings.TrimSpace(strings.ToLower(mode))
	if webMode == "" {
		webMode = "https"
	}
	if strings.TrimSpace(allowedDomains) != "" {
		parts := strings.Split(allowedDomains, ",")
		list := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(strings.ToLower(p))
			if p != "" {
				list = append(list, p)
			}
		}
		webDomains = list
	} else {
		webDomains = nil
	}
}

func (e *Executor) WebFetch(ctx context.Context, rawURL string, maxBytes int) (string, error) {
	if !webFetchEnabled() {
		return "", fmt.Errorf("web_fetch is disabled (set GOGEN_WEB_FETCH=on to re-enable)")
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}

	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	if maxBytes > webFetchHardMax {
		maxBytes = webFetchHardMax
	}

	parsed, err := validateFetchURL(rawURL)
	if err != nil {
		return "", err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	body, finalURL, err := fetchHTTP(ctx, parsed.String(), maxBytes)
	if err != nil {
		return "", err
	}

	text := htmlToText(body)

	// Build result.
	var b strings.Builder
	if finalURL != parsed.String() {
		fmt.Fprintf(&b, "Final URL (after redirects): %s\n\n", finalURL)
	}
	if len(text) > webFetchOutputLimit {
		fmt.Fprintf(&b, "Content (first %d of %d bytes):\n", webFetchOutputLimit, len(text))
		b.WriteString(text[:webFetchOutputLimit])
		fmt.Fprintf(&b, "\n\n… truncated (%d bytes total)", len(text))
	} else if text == "" {
		b.WriteString("(empty body)")
	} else {
		b.WriteString(text)
	}
	return b.String(), nil
}

// webFetchEnabled reports whether the web_fetch tool is active.
func webFetchEnabled() bool {
	webMu.RLock()
	if webEnabled != nil {
		e := *webEnabled
		webMu.RUnlock()
		return e
	}
	webMu.RUnlock()
	raw := strings.TrimSpace(os.Getenv("GOGEN_WEB_FETCH"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("GOGEN_WEB_SEARCH"))
	}
	return strings.EqualFold(raw, "on") || strings.EqualFold(raw, "1") || strings.EqualFold(raw, "true")
}

func fetchMode() string {
	webMu.RLock()
	if webEnabled != nil {
		m := webMode
		webMu.RUnlock()
		return m
	}
	webMu.RUnlock()
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOGEN_WEB_FETCH_MODE")))
	if mode == "" {
		mode = "https"
	}
	return mode
}

func fetchAllowedDomains() []string {
	webMu.RLock()
	if webEnabled != nil {
		d := webDomains
		webMu.RUnlock()
		return d
	}
	webMu.RUnlock()
	raw := strings.TrimSpace(os.Getenv("GOGEN_WEB_ALLOWED_DOMAINS"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validateFetchURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	// Block private/internal hosts on the initial URL (redirects are also checked).
	if isPrivateHost(u.Host) {
		return nil, fmt.Errorf("requests to private/internal hosts are blocked: %s", u.Hostname())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("web_fetch only supports http/https URLs (got %q)", u.Scheme)
	}
	// Enforce HTTPS-only unless explicitly allowed.
	if fetchMode() == "https" && u.Scheme != "https" {
		return nil, fmt.Errorf("web_fetch requires https (got %s). Set GOGEN_WEB_FETCH_MODE=all for http", u.Scheme)
	}
	if allowedDomains := fetchAllowedDomains(); len(allowedDomains) > 0 {
		ok := false
		host := strings.ToLower(u.Hostname())
		for _, d := range allowedDomains {
			if host == d || strings.HasSuffix(host, "."+d) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("domain %q is not in GOGEN_WEB_ALLOWED_DOMAINS", u.Hostname())
		}
	}
	return u, nil
}

func isPrivateHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "localhost" || host == "localhost.localdomain" {
		return true
	}
	if fetchPrivateRE.MatchString(host) {
		return true
	}
	if colon := strings.LastIndex(host, ":"); colon > 0 {
		host = host[:colon]
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return true // can't resolve, be safe
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}

func fetchHTTP(ctx context.Context, rawURL string, maxBytes int) ([]byte, string, error) {
	return fetchHTTPWithUA(ctx, rawURL, "gogen/1.0", maxBytes)
}

// fetchHTTPWithUA is like fetchHTTP but uses a custom User-Agent string.
func fetchHTTPWithUA(ctx context.Context, rawURL, userAgent string, maxBytes int) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webFetchMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			nextURL := req.URL.String()
			for _, prev := range via {
				if prev.URL.String() == nextURL {
					return fmt.Errorf("redirect loop detected: %s", nextURL)
				}
			}
			if isPrivateHost(req.URL.Host) {
				return fmt.Errorf("redirect to private host blocked: %s", req.URL.Host)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, rawURL, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return nil, rawURL, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.Request.URL.String(), fmt.Errorf("http %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(maxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.Request.URL.String(), fmt.Errorf("read body: %w", err)
	}
	return body, resp.Request.URL.String(), nil
}

// fetchHTTPPost performs an HTTP POST with form-encoded body. Used by DDG Lite
// search to avoid CAPTCHA (GET requests to DDG Lite trigger bot detection).
func fetchHTTPPost(ctx context.Context, rawURL, formBody string, maxBytes int) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webFetchMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			nextURL := req.URL.String()
			for _, prev := range via {
				if prev.URL.String() == nextURL {
					return fmt.Errorf("redirect loop detected: %s", nextURL)
				}
			}
			if isPrivateHost(req.URL.Host) {
				return fmt.Errorf("redirect to private host blocked: %s", req.URL.Host)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL,
		strings.NewReader(formBody))
	if err != nil {
		return nil, rawURL, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("User-Agent", "gogen/1.0")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,text/plain")

	resp, err := client.Do(req)
	if err != nil {
		return nil, rawURL, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.Request.URL.String(), fmt.Errorf("http %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(maxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.Request.URL.String(), fmt.Errorf("read body: %w", err)
	}
	return body, resp.Request.URL.String(), nil
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
