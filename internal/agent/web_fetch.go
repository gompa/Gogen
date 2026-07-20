package agent

import (
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
)

const (
	webFetchTimeout      = 15 * time.Second
	webFetchDefaultMax   = 64 * 1024 // 64 KB — plenty for docs pages
	webFetchHardMax      = 2 * 1024 * 1024
	webFetchMaxRedirects = 3
	webFetchOutputLimit  = 64 * 1024 // max bytes returned in the tool result string
)

var fetchPrivateRE = regexp.MustCompile(`^127\.|^10\.|^172\.(1[6-9]|2[0-9]|3[01])\.|^192\.168\.|^0\.0\.0\.0$|^::1$|^fe80:`)

// webCfg holds all runtime web fetch/search configuration behind a single mutex.
var webCfg webCfgState

type webCfgState struct {
	mu            sync.RWMutex
	fetchOn       *bool   // nil until configured
	searchOn      *bool   // nil until configured
	fetchMode     string  // "https" or "all"
	fetchDomains  []string // domain allowlist for fetch
	searchBackend string  // "brave" or ""
	searchAPIKey  string
}

func (c *webCfgState) isFetchOn() bool {
	c.mu.RLock()
	if c.fetchOn != nil {
		v := *c.fetchOn
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	raw := strings.TrimSpace(os.Getenv("GOGEN_WEB_FETCH"))
	return strings.EqualFold(raw, "on") || strings.EqualFold(raw, "1") || strings.EqualFold(raw, "true")
}

func (c *webCfgState) isSearchOn() bool {
	c.mu.RLock()
	if c.searchOn != nil {
		v := *c.searchOn
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	raw := strings.TrimSpace(os.Getenv("GOGEN_WEB_SEARCH"))
	return strings.EqualFold(raw, "on") || strings.EqualFold(raw, "1") || strings.EqualFold(raw, "true")
}

func (c *webCfgState) mode() string {
	c.mu.RLock()
	if c.fetchOn != nil || c.searchOn != nil {
		m := c.fetchMode
		c.mu.RUnlock()
		return m
	}
	c.mu.RUnlock()
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOGEN_WEB_FETCH_MODE")))
	if mode == "" {
		mode = "https"
	}
	return mode
}

func (c *webCfgState) allowedDomains() []string {
	c.mu.RLock()
	if c.fetchOn != nil || c.searchOn != nil {
		d := c.fetchDomains
		c.mu.RUnlock()
		return d
	}
	c.mu.RUnlock()
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

func (c *webCfgState) searchBE() string {
	c.mu.RLock()
	b := c.searchBackend
	c.mu.RUnlock()
	return b
}

func (c *webCfgState) searchKey() string {
	c.mu.RLock()
	k := c.searchAPIKey
	c.mu.RUnlock()
	return k
}

// ConfigureWebFetch applies runtime web fetch settings from merged config.
func ConfigureWebFetch(enabled bool, mode string, allowedDomains string) {
	webCfg.mu.Lock()
	defer webCfg.mu.Unlock()
	webCfg.fetchOn = &enabled
	webCfg.fetchMode = strings.TrimSpace(strings.ToLower(mode))
	if webCfg.fetchMode == "" {
		webCfg.fetchMode = "https"
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
		webCfg.fetchDomains = list
	} else {
		webCfg.fetchDomains = nil
	}
}

// ConfigureWebSearchEnabled sets whether web_search is allowed (independent of web_fetch).
func ConfigureWebSearchEnabled(enabled bool) {
	webCfg.mu.Lock()
	defer webCfg.mu.Unlock()
	webCfg.searchOn = &enabled
}

// ConfigureWebSearch applies runtime web search settings from merged config.
func ConfigureWebSearch(backend, apiKey string) {
	webCfg.mu.Lock()
	defer webCfg.mu.Unlock()
	webCfg.searchBackend = strings.ToLower(strings.TrimSpace(backend))
	webCfg.searchAPIKey = strings.TrimSpace(apiKey)
}

// sharedFetchClient is reused across requests for connection pooling.
// CheckRedirect enforces max redirects, loop detection, and private-host blocking.
var sharedFetchClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 90 * time.Second,
		DialContext:     dialContextPublicOnly,
	},
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

// fetchRequest describes a single HTTP fetch operation.
type fetchRequest struct {
	URL      string // target URL
	Method   string // "GET" (default) or "POST"
	UA       string // User-Agent (default "gogen/1.0")
	Body     string // form-encoded body for POST requests
	MaxBytes int    // max response body bytes to read
}

// doFetch performs a single HTTP request with SSRF protection and redirect
// enforcement. It uses the shared client (dialContextPublicOnly + CheckRedirect)
// so private/internal hosts are blocked at both the dial and redirect level.
func doFetch(ctx context.Context, req fetchRequest) ([]byte, string, error) {
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	ua := req.UA
	if ua == "" {
		ua = "gogen/1.0"
	}

	var httpReq *http.Request
	var err error

	if req.Method == http.MethodPost {
		httpReq, err = http.NewRequestWithContext(ctx, http.MethodPost, req.URL,
			strings.NewReader(req.Body))
	} else {
		httpReq, err = http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	}
	if err != nil {
		return nil, req.URL, fmt.Errorf("request: %w", err)
	}

	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml")
	if req.Method == http.MethodPost {
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := sharedFetchClient.Do(httpReq)
	if err != nil {
		return nil, req.URL, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.Request.URL.String(), fmt.Errorf("http %d", resp.StatusCode)
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	limited := io.LimitReader(resp.Body, int64(maxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.Request.URL.String(), fmt.Errorf("read body: %w", err)
	}
	return body, resp.Request.URL.String(), nil
}

func dialContextPublicOnly(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ipAddr := range ips {
		ip := ipAddr.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, fmt.Errorf("requests to private/internal hosts are blocked: %s", ip)
		}
	}
	// Dial the original host:port so TLS SNI and HTTP/2 connection reuse keep
	// working. IP allowlisting above rejects private targets before dial.
	d := net.Dialer{Timeout: webFetchTimeout}
	return d.DialContext(ctx, network, addr)
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

func (e *Executor) WebFetch(ctx context.Context, rawURL string, maxBytes int) (string, error) {
	if !webCfg.isFetchOn() {
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

	body, finalURL, err := doFetch(ctx, fetchRequest{
		URL:      parsed.String(),
		MaxBytes: maxBytes,
	})
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
	if webCfg.mode() == "https" && u.Scheme != "https" {
		return nil, fmt.Errorf("web_fetch requires https (got %s). Set GOGEN_WEB_FETCH_MODE=all for http", u.Scheme)
	}
	if allowedDomains := webCfg.allowedDomains(); len(allowedDomains) > 0 {
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
