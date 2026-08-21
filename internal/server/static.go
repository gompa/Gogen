package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed all:web
var webAssets embed.FS

// staticAsset is a lazily compressed embedded asset served by HandleStatic.
// raw and gzip are cached after first use so a page load does not re-compress
// the multi-MB Monaco bundle on every request.
type staticAsset struct {
	contentType string
	etag        string // weak ETag derived from the raw content hash
	raw         []byte
	gzip        []byte // nil when the asset is not worth gzip-ing
}

// staticAssetCache caches embedded assets after their first request. Entries
// are immutable once stored, so readers can use the returned pointer freely
// after the mutex is released.
type staticAssetCache struct {
	mu      sync.RWMutex
	entries map[string]*staticAsset
}

// peek returns the cached asset for name without reading or compressing anything.
func (c *staticAssetCache) peek(name string) (*staticAsset, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.entries[name]
	return a, ok
}

// get returns the cached asset for name, filling it from content on first use.
// gzipable + minGzipSize control whether gzip bytes are produced (gzip when
// len(content) > minGzipSize); pass minGzipSize = -1 to compress any size.
func (c *staticAssetCache) get(name string, content []byte, contentType string, gzipable bool, minGzipSize int) *staticAsset {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.entries[name]; ok {
		return a
	}
	a := &staticAsset{contentType: contentType, raw: content, etag: weakETag(content)}
	if gzipable && len(content) > minGzipSize {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(content)
		_ = gz.Close()
		a.gzip = buf.Bytes()
	}
	if c.entries == nil {
		c.entries = make(map[string]*staticAsset)
	}
	c.entries[name] = a
	return a
}

// weakETag returns a weak validator for content. Weak is used because the same
// entity is served both identity- and gzip-encoded (differing bytes but equal
// semantics); Vary: Accept-Encoding keeps the variants apart in caches.
func weakETag(content []byte) string {
	sum := sha256.Sum256(content)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches reports whether an If-None-Match header matches etag (exact
// match, one of several comma-separated candidates, or a `*` wildcard).
func etagMatches(header, etag string) bool {
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == etag {
			return true
		}
	}
	return false
}

func (s *Server) HandleStatic(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	// Bootstrap: accept the onboarding code in the URL path (/pair/<code> —
	// the QR form: some phone camera apps and in-app browsers strip query
	// strings, so the QR carries the code in the path, which survives) or in
	// the query (?pair=, ?token=), set the HttpOnly cookie, redirect without
	// the secret. All forms are per-IP rate limited: they are the only
	// unauthenticated endpoints that check a secret.
	if s.authToken != "" {
		if code := pairingCodeFromPath(r.URL.Path); code != "" {
			// Only a real navigation may consume the code: camera-app
			// link previews fetch the URL themselves (no-cors) and would
			// burn a use and set the cookie into a throwaway context,
			// leaving the browser that opens the link without a session.
			if !consumeAllowed(r) {
				log.Printf("pairing: skipped non-navigation request from %s (path /pair/<code>, %s)", clientIP(r), reqSig(r))
				pairingPreviewResponse(w)
				return
			}
			s.bootstrapPairing(w, r, code, "path /pair/<code>", "/", secure)
			return
		}
		q := r.URL.Query()
		if tok := strings.TrimSpace(q.Get("token")); tok != "" {
			if !s.allowBootstrap(r) {
				log.Printf("pairing: rate-limited from %s (query ?token=)", clientIP(r))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			if tokenMatches(tok, s.authToken) {
				log.Printf("pairing: token bootstrap accepted from %s", clientIP(r))
				setAuthCookie(w, s.authToken, secure)
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
			log.Printf("pairing: token bootstrap rejected from %s", clientIP(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if code := strings.TrimSpace(q.Get("pair")); code != "" {
			if !consumeAllowed(r) {
				log.Printf("pairing: skipped non-navigation request from %s (query ?pair=, %s)", clientIP(r), reqSig(r))
				pairingPreviewResponse(w)
				return
			}
			s.bootstrapPairing(w, r, code, "query ?pair=", r.URL.Path, secure)
			return
		}
	}
	if !s.checkAuth(r) {
		// No (valid) credentials at all — e.g. a phone scanner that
		// stripped the ?pair= query. Serve the pairing page so the code can
		// be typed in, instead of a bare 401.
		//
		// Log every such request — the only prompt path that is otherwise
		// silent — so a "pairing: accepted" line followed by a prompt can be
		// diagnosed from the server side: cookie absent means the browser
		// never stored or sent it (camera-app WebView jar split, SameSite
		// withholding, different browser/device); cookie present but
		// rejected means the server's token rotated (ephemeral token /
		// different working dir) or the request hit a different server
		// instance. Never log the credential itself.
		_, cookieErr := r.Cookie(authCookieName)
		// cookies sent: the count of cookies in the request — 0 means this
		// context's cookie jar is blocked or ephemeral for this origin;
		// >0 with our cookie absent means the jar works but the pairing
		// cookie was never stored (partitioned storage, attribute reject).
		// referer + sec-fetch classify the request: a real browser
		// navigation is navigate/document (same-origin on redirect
		// follow), a programmatic or preview fetch is cors/no-cors/empty —
		// the difference between "Brave loaded the page" and "an embedded
		// fetcher consumed the pairing code".
		log.Printf("auth: unauthenticated page request from %s (path %q, auth cookie present: %v, cookies sent: %d, secure: %v, referer: %q, %s, user-agent: %q)", clientIP(r), r.URL.Path, cookieErr == nil, len(r.Cookies()), secure, shortReferer(r), reqSig(r), shortUA(r))
		// The decisive diagnosis: an exchange was accepted from this same
		// device moments ago (notePairingAccept) and yet no cookie came
		// back — the server held up its end (the cookie is set to the very
		// token checkAuth compares against), so the browser either scanned
		// into a different cookie jar than the one now loading "/" (camera
		// app / in-app browser vs. the real browser) or is blocking cookies
		// for this address (Brave Shields, private tab). Say so, both in
		// the log and on the page — the generic "enter the code" prompt
		// sends the user down a path (typing the code) that cannot work
		// while cookies are dropped.
		if at, ok := s.recentPairingAccept(clientIP(r)); ok && cookieErr != nil {
			log.Printf("auth: pairing was accepted from %s at %s but this request carries no cookie — the browser dropped or withheld it (different cookie jar or blocked cookies). Typing the code cannot help until cookies are allowed for this address.", clientIP(r), at.Local().Format("15:04:05"))
			servePairingPage(w, pairingMsgCookieLost)
			return
		}
		servePairingPage(w, pairingMsgEnterCode)
		return
	}

	path := r.URL.Path
	if path == "/" || path == "" {
		// index.html is compressed regardless of size (it is the bootstrap
		// page); everything else follows the staticGzipMime + 512-byte rule.
		s.serveEmbedded(w, r, "web/index.html", "text/html; charset=utf-8", "no-cache", true)
		return
	}

	// Serve embedded assets under /monaco/... (and future static paths).
	rel := strings.TrimPrefix(path, "/")
	if hasPathTraversal(rel) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := "web/" + rel
	ct := contentTypeForExt(filepath.Ext(name))
	// no-cache: the UI assets are embedded in the binary and change with every
	// rebuild; a 24h max-age kept browsers serving stale app.js (the "rebuilt
	// but the fix isn't showing" class of bugs). The ETag below still yields
	// 304s when the bytes are unchanged, so reloads stay cheap.
	s.serveEmbedded(w, r, name, ct, "no-cache", false)
}

// bootstrapPairing validates a pairing-code candidate from an
// unauthenticated request and completes the exchange: on acceptance it sets
// the auth cookie and redirects to redirectTo; on rejection it serves the
// pairing page. Every exchange is logged — the code itself never is, only
// the outcome — so a failed scan can be diagnosed server-side. It always
// writes a response.
func (s *Server) bootstrapPairing(w http.ResponseWriter, r *http.Request, code, form, redirectTo string, secure bool) {
	if !s.allowBootstrap(r) {
		log.Printf("pairing: rate-limited from %s (%s)", clientIP(r), form)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	switch reason := s.consumePairingCode(code); reason {
	case pairingAccepted:
		log.Printf("pairing: accepted from %s (%s, secure: %v, referer: %q, %s, user-agent: %q)", clientIP(r), form, secure, shortReferer(r), reqSig(r), shortUA(r))
		s.notePairingAccept(clientIP(r))
		setAuthCookie(w, s.authToken, secure)
		http.Redirect(w, r, redirectTo, http.StatusFound)
	default:
		// reqSig + user-agent on rejections too: a stale link re-opened from
		// browser history is distinguishable from a fresh mis-scanned QR
		// only by its request shape, and the code is never logged.
		log.Printf("pairing: rejected (%s) from %s (%s, %s, user-agent: %q)", reason, clientIP(r), form, reqSig(r), shortUA(r))
		servePairingPage(w, pairingFailureMessage(reason)+s.pairingDiagnostic(reason))
	}
}

// allowBootstrap rate-limits the unauthenticated secret-checking bootstrap
// endpoints (?token=, ?pair=, /pair/<code>) per source IP. A nil limiter
// (bare test servers) means no throttling.
func (s *Server) allowBootstrap(r *http.Request) bool {
	return s.bootstrapLimiter == nil || s.bootstrapLimiter.allow(clientIP(r))
}

// consumeAllowed reports whether a request may consume a pairing code:
// only a GET that is a real top-level browser navigation.
//
// Sec-Fetch-Mode is authoritative when present (HTTPS or loopback — the
// origins browsers consider potentially trustworthy): every mainstream
// browser sends Sec-Fetch-Mode: navigate on top-level loads there, and
// camera-app link previews surface as no-cors/cors — require "navigate"
// outright.
//
// But the QR's own URL is plain http://<lan-ip>:<port>/pair/<code>
// (run_web.go), and browsers send NO Sec-Fetch-* headers at all to origins
// that are not potentially trustworthy (fetch metadata is withheld from
// plain-HTTP requests). A real phone scan therefore arrives header-less,
// and any additional per-browser assumption about what such a request
// "must" carry (Upgrade-Insecure-Requests, Sec-CH-UA, …) risks locking out
// that browser — which is how the strict no-fallback rule broke every
// plain-HTTP scan in the first place. plainHTTPNavigation thus relies only
// on the two signals that are stable across every observed browser and
// every mainstream preview client, and errs on the side of allowing: a
// preview that slips through burns one of the code's maxPairingUses (the
// real navigation that follows still logs in), whereas a real browser
// wrongly skipped cannot log in at all.
func consumeAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return plainHTTPNavigation(r)
}

// plainHTTPNavigation classifies a request carrying no Sec-Fetch-* headers
// (a top-level navigation over plain HTTP — the QR form — or a raw HTTP
// client) as a browser navigation, using only signals that hold across all
// mainstream browsers:
//
//   - no X-Requested-With — set by Android WebView app containers (camera
//     apps' in-app browsers), never by a standalone browser;
//   - Accept starting with "text/html" — every browser puts text/html
//     first when navigating a document; raw preview clients (okhttp,
//     Dalvik, curl, image fetchers) send */* or nothing.
//
// Upgrade-Insecure-Requests is deliberately NOT required: it is a client
// preference some browser configurations omit, and requiring it locked
// out real scans. It is still logged (reqSig) for diagnosis.
func plainHTTPNavigation(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") != "" {
		return false
	}
	return strings.HasPrefix(r.Header.Get("Accept"), "text/html")
}

// pairingPreviewResponse answers a skipped non-navigation request: no
// cookie, no redirect, no pairing use consumed. It serves the full sign-in
// page rather than bare text so the request can never dead-end: a
// camera-app preview renders a form it cannot use (harmless), while a real
// browser that was misclassified — the failure mode that locked phones out
// of QR login — gets the manual code-entry form and can still sign in.
// 200 OK: no secret was checked, this is not an auth failure.
func pairingPreviewResponse(w http.ResponseWriter) {
	renderPairingPage(w, http.StatusOK, pairingMsgPreview)
}

// pairingCodeFromPath extracts an onboarding code from a /pair/<code> URL
// path — the form the QR encodes, because some phone camera apps and
// in-app browsers strip query strings when opening a scanned URL, and the
// path is what survives. A trailing slash is tolerated (some browsers
// normalize scanned URLs to directory form); any other path shape returns
// "" (the request then falls through to the normal flow).
func pairingCodeFromPath(path string) string {
	const prefix = "/pair/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	code := strings.TrimSpace(strings.TrimPrefix(path, prefix))
	// Tolerate a single trailing slash. Anything else containing a slash
	// is still rejected (e.g. /pair/a/b).
	code = strings.TrimSuffix(code, "/")
	if code == "" || strings.Contains(code, "/") || code == "." || code == ".." || len(code) > 128 {
		return ""
	}
	return code
}

// shortHeader returns a request header truncated for log lines. Never
// contains credentials.
func shortHeader(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if len(v) > 128 {
		v = v[:128]
	}
	return v
}

// shortUA returns the request's User-Agent truncated for log lines, so the
// client that scanned the QR can be correlated with the client that later
// hits "/" without a cookie (camera-app WebView vs. system browser).
func shortUA(r *http.Request) string {
	return shortHeader(r, "User-Agent")
}

// shortReferer returns the request's Referer truncated for log lines, so a
// redirect follow (same-origin referer) can be told apart from an
// intent-opened tab (no referer) or a preview fetch (app referer).
func shortReferer(r *http.Request) string {
	return shortHeader(r, "Referer")
}

// reqSig renders the request-classification fields for log lines: the
// Sec-Fetch-* triple, Sec-CH-UA (client-hints browser brand), Accept,
// Upgrade-Insecure-Requests and X-Requested-With. A real browser
// navigation is navigate/document with Accept: text/html; an app's raw
// HTTP client sends no Sec-Fetch-* at all and often Accept: */* — but so
// does a real browser over plain HTTP (the QR form), which is why the
// plain-HTTP fallback keys on Accept and X-Requested-With only. The
// remaining fields are logged purely for diagnosis (they named the client
// that scanned vs. the one that hit "/" in past bug hunts): Sec-CH-UA
// names the browser brand (e.g. Brave); X-Requested-With names the Android
// app whose WebView made the request; Upgrade-Insecure-Requests is a
// browser-only preference whose absence in a real scan must NOT be treated
// as proof of a preview.
func reqSig(r *http.Request) string {
	return fmt.Sprintf("sec-fetch: %q/%q/%q, sec-ch-ua: %q, accept: %q, uir: %q, x-requested-with: %q",
		r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest"),
		shortHeader(r, "Sec-CH-UA"), shortHeader(r, "Accept"),
		shortHeader(r, "Upgrade-Insecure-Requests"), shortHeader(r, "X-Requested-With"))
}

// pairingPage holds the embedded sign-in page (web/pair.html), loaded once.
// The page is fully self-contained — every other asset under / is
// auth-gated, so it cannot reference external CSS/JS.
var pairingPage = sync.OnceValue(func() []byte {
	b, err := webAssets.ReadFile("web/pair.html")
	if err != nil {
		panic("embedded web/pair.html missing: " + err.Error())
	}
	return b
})

// servePairingPage renders the unauthenticated sign-in page with a
// server-generated message (never user input — the submitted code is not
// echoed) and a form that re-submits the code through the normal ?pair=
// bootstrap. Always 401: the exchange is still unauthorized, the body just
// tells the user how to fix it.
func servePairingPage(w http.ResponseWriter, msg string) {
	renderPairingPage(w, http.StatusUnauthorized, msg)
}

// renderPairingPage writes the sign-in page with the given status and a
// server-generated message. The form it carries re-submits a typed code
// through the normal ?pair= bootstrap, so every path that renders this page
// — auth failure, rejected exchange, skipped "preview" request — leaves the
// user a way in.
func renderPairingPage(w http.ResponseWriter, status int, msg string) {
	// Replace every occurrence: the template must not ship an unreplaced
	// placeholder to the client, wherever it appears in future edits.
	page := strings.Replace(string(pairingPage()), "{{MESSAGE}}", msg, -1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(page))
}

// hasPathTraversal reports whether any path element is ".." (a traversal
// attempt). A substring check would also reject legitimate asset names that
// contain ".." (e.g. "foo..bar.js"); only exact ".." elements escape the
// embedded tree. The embed.FS lookup itself rejects ".." elements too, so
// this is defense-in-depth with a clearer 404.
func hasPathTraversal(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// serveEmbedded serves an embedded asset from the static asset cache, reading
// and gzip-compressing it on first use only. Repeated requests reuse the
// cached bytes and revalidate via ETag/If-None-Match (304), so a page reload
// never re-compresses the multi-MB Monaco bundle.
func (s *Server) serveEmbedded(w http.ResponseWriter, r *http.Request, name, contentType, cacheControl string, gzipAlways bool) {
	asset, ok := s.staticAssets.peek(name)
	if !ok {
		content, err := webAssets.ReadFile(name)
		if err != nil {
			if gzipAlways {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				http.Error(w, "not found", http.StatusNotFound)
			}
			return
		}
		minGzipSize := 512
		if gzipAlways {
			minGzipSize = -1
		}
		asset = s.staticAssets.get(name, content, contentType, staticGzipMime(contentType), minGzipSize)
	}

	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", asset.etag)

	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && etagMatches(match, asset.etag) {
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if asset.gzip != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		_, _ = w.Write(asset.gzip)
		return
	}
	_, _ = w.Write(asset.raw)
}

// staticGzipMime reports whether content of the given type is worth gzip-ing
// on the fly. Binary/already-compressed assets (fonts, images) are excluded.
func staticGzipMime(ct string) bool {
	switch ct {
	case "text/html; charset=utf-8", "text/css; charset=utf-8",
		"text/javascript; charset=utf-8", "application/javascript",
		"application/json; charset=utf-8", "image/svg+xml":
		return true
	}
	return false
}

func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".ttf":
		return "font/ttf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".map":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}
