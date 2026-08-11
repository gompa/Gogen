package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	// Bootstrap: accept ?token= once, set HttpOnly cookie, redirect without query.
	if s.authToken != "" {
		if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
			if q == s.authToken {
				setAuthCookie(w, s.authToken, secure)
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
