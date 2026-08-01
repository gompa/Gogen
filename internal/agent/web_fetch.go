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

	"golang.org/x/net/http2"
)

const (
	webFetchTimeout      = 15 * time.Second
	webFetchDefaultMax   = 64 * 1024 // 64 KB — plenty for docs pages
	webFetchHardMax      = 2 * 1024 * 1024
	webFetchMaxRedirects = 3
	// webFetchOutputLimit caps the tool-result string. It is kept equal to
	// webFetchHardMax so an explicit max_bytes request is never silently cut
	// short by a second, smaller cap: the body-read cap reports truncation
	// instead (see doFetch). Default fetches stay at webFetchDefaultMax,
	// which is small enough that the truncation notice fits under
	// MaxToolResultBytes and passes through to the model intact.
	// The output-limit branch below is still reachable: markdown conversion
	// can expand a body (escaping, link/code markup), so a full-size 2 MB
	// body can yield more than 2 MB of text.
	webFetchOutputLimit = webFetchHardMax
)

var fetchPrivateRE = regexp.MustCompile(`^127\.|^10\.|^172\.(1[6-9]|2[0-9]|3[01])\.|^192\.168\.|^0\.0\.0\.0$|^::1$|^fe80:`)

// fetchURLValidator is swappable in tests that exercise the fetch/download
// paths against a local httptest server (the real validator blocks private
// hosts, which is exactly what a local test server is).
var fetchURLValidator = validateFetchURL

// webCfg holds all runtime web fetch/search configuration behind a single mutex.
var webCfg webCfgState

// envDefaults caches env-var lookups once at first access to avoid per-call syscalls.
var envDefaults struct {
	fetchOn       bool
	searchOn      bool
	fetchMode     string
	fetchModeOnce sync.Once
}

// webCfgState holds all runtime web fetch/search configuration behind a single mutex.
type webCfgState struct {
	mu            sync.RWMutex
	fetchOn       *bool    // nil until configured
	searchOn      *bool    // nil until configured
	fetchMode     string   // "https" or "all"
	fetchDomains  []string // domain allowlist for fetch
	searchBackend string   // "brave" or ""
	searchAPIKey  string
}

// envToggle is a thread-safe one-shot boolean env-var reader.
// Each variable gets its own instance so they cache independently.
type envToggle struct {
	once sync.Once
	val  bool
}

// get reads envVar on first call and caches the boolean result.
func (t *envToggle) get(envVar string) bool {
	t.once.Do(func() {
		raw := strings.TrimSpace(os.Getenv(envVar))
		t.val = strings.EqualFold(raw, "on") || strings.EqualFold(raw, "1") || strings.EqualFold(raw, "true")
	})
	return t.val
}

var fetchOnToggle, searchOnToggle envToggle

func envFetchOn() bool  { return fetchOnToggle.get("GOGEN_WEB_FETCH") }
func envSearchOn() bool { return searchOnToggle.get("GOGEN_WEB_SEARCH") }

// envFetchMode returns the web fetch mode from env. Cached after first read.
func envFetchMode() string {
	envDefaults.fetchModeOnce.Do(func() {
		mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOGEN_WEB_FETCH_MODE")))
		if mode == "" {
			mode = "https"
		}
		envDefaults.fetchMode = mode
	})
	return envDefaults.fetchMode
}

func (c *webCfgState) isFetchOn() bool {
	c.mu.RLock()
	if c.fetchOn != nil {
		v := *c.fetchOn
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	return envFetchOn()
}

func (c *webCfgState) isSearchOn() bool {
	c.mu.RLock()
	if c.searchOn != nil {
		v := *c.searchOn
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	return envSearchOn()
}

func (c *webCfgState) mode() string {
	c.mu.RLock()
	if c.fetchOn != nil || c.searchOn != nil {
		m := c.fetchMode
		c.mu.RUnlock()
		return m
	}
	c.mu.RUnlock()
	return envFetchMode()
}

func (c *webCfgState) allowedDomains() []string {
	c.mu.RLock()
	if c.fetchOn != nil || c.searchOn != nil {
		d := c.fetchDomains
		c.mu.RUnlock()
		return d
	}
	c.mu.RUnlock()
	return envAllowedDomains()
}

// parseDomainList splits a comma-separated domain allowlist, trimming,
// lowercasing, and dropping empty entries. Returns nil for empty input.
func parseDomainList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
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

// envAllowedDomains parses and caches GOGEN_WEB_ALLOWED_DOMAINS from env.
var envAllowedDomainsVal []string
var envAllowedDomainsOnce sync.Once

func envAllowedDomains() []string {
	envAllowedDomainsOnce.Do(func() {
		envAllowedDomainsVal = parseDomainList(os.Getenv("GOGEN_WEB_ALLOWED_DOMAINS"))
	})
	return envAllowedDomainsVal
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
	webCfg.fetchDomains = parseDomainList(allowedDomains)
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
	Transport: func() *http.Transport {
		tr := &http.Transport{
			MaxIdleConns:    10,
			IdleConnTimeout: 90 * time.Second,
			DialContext:     dialContextPublicOnly,
		}
		if err := http2.ConfigureTransport(tr); err != nil {
			panic(fmt.Sprintf("http2: %v", err))
		}
		return tr
	}(),
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
// It returns the response body, its Content-Type header, the final URL after
// redirects, and whether the body was cut short by req.MaxBytes.
func doFetch(ctx context.Context, req fetchRequest) ([]byte, string, string, bool, error) {
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
		return nil, "", req.URL, false, fmt.Errorf("request: %w", err)
	}

	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml")
	if req.Method == http.MethodPost {
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := sharedFetchClient.Do(httpReq)
	if err != nil {
		return nil, "", req.URL, false, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", resp.Request.URL.String(), false, fmt.Errorf("http %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	// Read one byte past the cap so truncation is detectable instead of
	// silently looking like the complete document (which previously made
	// mid-file cuts indistinguishable from full files).
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", resp.Request.URL.String(), false, fmt.Errorf("read body: %w", err)
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	return body, contentType, resp.Request.URL.String(), truncated, nil
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

// WebFetchOptions configures a web_fetch call. Selector and Query are the
// agent-driven extraction primitives for unseen pages: instead of relying
// on pre-tuned boilerplate removal, the caller names exactly what it wants
// (see extractBySelector and searchExtractedText).
type WebFetchOptions struct {
	// MaxBytes caps the response body read (default webFetchDefaultMax,
	// capped at webFetchHardMax).
	MaxBytes int
	// Selector is an optional CSS selector: when set, only matching
	// elements are converted to markdown.
	Selector string
	// Query is an optional case-insensitive text search over the extracted
	// content: when set, matches with surrounding context lines are
	// returned instead of the full text. Composes with Selector.
	Query string
	// Context is the number of context lines around each Query match
	// (default searchDefaultContext, capped at searchMaxContextLines).
	Context int
}

func (e *Executor) WebFetch(ctx context.Context, rawURL string, opts WebFetchOptions) (string, error) {
	if !webCfg.isFetchOn() {
		return "", fmt.Errorf("web_fetch is disabled (set GOGEN_WEB_FETCH=on to re-enable)")
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}

	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	if maxBytes > webFetchHardMax {
		maxBytes = webFetchHardMax
	}

	parsed, err := fetchURLValidator(rawURL)
	if err != nil {
		return "", err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	body, contentType, finalURL, truncated, err := doFetch(ctx, fetchRequest{
		URL:      parsed.String(),
		MaxBytes: maxBytes,
	})
	if err != nil {
		return "", err
	}

	var text string
	if opts.Selector != "" {
		text, err = extractBySelector(body, opts.Selector, finalURL)
		if err != nil {
			return "", err
		}
	} else {
		text = extractResponseText(contentType, finalURL, body)
	}

	if opts.Query != "" {
		result := searchExtractedText(text, opts.Query, opts.Context)
		if truncated {
			result = fmt.Sprintf("Note: body exceeded max_bytes and was cut before extraction; raise max_bytes (up to %d) if the match region is missing.\n\n", webFetchHardMax) + result
		}
		return result, nil
	}

	// Build result.
	var b strings.Builder
	if finalURL != parsed.String() {
		fmt.Fprintf(&b, "Final URL (after redirects): %s\n\n", finalURL)
	}
	if opts.Selector != "" && truncated {
		fmt.Fprintf(&b, "Note: body exceeded max_bytes and was cut before extraction; raise max_bytes (up to %d) if elements are missing.\n\n", webFetchHardMax)
	}
	switch {
	case truncated:
		// The body was cut by max_bytes. Report it explicitly — a silent
		// mid-file cut previously looked like the complete document, and
		// the marker keeps this result intact under MaxToolResultBytes.
		if opts.Selector == "" {
			fmt.Fprintf(&b, "Content (first %d of more than %d bytes):\n", len(text), maxBytes)
			b.WriteString(text)
			fmt.Fprintf(&b, "\n\n… truncated (body exceeds %d bytes; pass max_bytes up to %d to fetch more)", maxBytes, webFetchHardMax)
		} else {
			// Selector mode: the element list is already complete for the
			// bytes we read; just say the body was cut.
			b.WriteString(text)
			fmt.Fprintf(&b, "\n\n… body exceeded %d bytes and was cut before extraction (pass max_bytes up to %d to fetch more)", maxBytes, webFetchHardMax)
		}
	case len(text) > webFetchOutputLimit:
		fmt.Fprintf(&b, "Content (first %d of %d bytes):\n", webFetchOutputLimit, len(text))
		b.WriteString(text[:webFetchOutputLimit])
		fmt.Fprintf(&b, "\n\n… truncated (%d bytes total)", len(text))
	case text == "":
		b.WriteString("(empty body)")
	default:
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
