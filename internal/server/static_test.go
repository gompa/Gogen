package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleStaticGzipCacheAnd304 verifies that static assets are gzip-compressed
// once, served from cache on subsequent requests, and revalidated via
// ETag/If-None-Match (304) so repeated page loads never re-compress the bundle.
func TestHandleStaticGzipCacheAnd304(t *testing.T) {
	s := &Server{}

	// First request with gzip: must return valid gzip of the embedded file.
	req := httptest.NewRequest("GET", "/monaco/editor.main.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip encoding, got %q", rec.Header().Get("Content-Encoding"))
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header")
	}
	if !bytes.HasPrefix([]byte(etag), []byte(`W/"`)) {
		t.Fatalf("expected weak ETag, got %q", etag)
	}
	firstBody := rec.Body.Bytes()
	raw, err := webAssets.ReadFile("web/monaco/editor.main.css")
	if err != nil {
		t.Fatal(err)
	}
	unzipped := mustGunzip(t, firstBody)
	if !bytes.Equal(unzipped, raw) {
		t.Fatal("gzip body does not match embedded asset")
	}

	// Second request must serve the cached compressed bytes (identical output).
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status=%d, want 200", rec2.Code)
	}
	if !bytes.Equal(firstBody, rec2.Body.Bytes()) {
		t.Fatal("second request did not reuse the cached gzip bytes")
	}

	// If-None-Match with the ETag revalidates to 304 with no body.
	req304 := httptest.NewRequest("GET", "/monaco/editor.main.css", nil)
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	s.HandleStatic(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("revalidation status=%d, want 304", rec304.Code)
	}
	if rec304.Body.Len() != 0 {
		t.Fatalf("304 must have no body, got %d bytes", rec304.Body.Len())
	}
	if rec304.Header().Get("ETag") != etag {
		t.Fatalf("304 ETag=%q, want %q", rec304.Header().Get("ETag"), etag)
	}

	// A stale/wrong ETag must serve the full body.
	reqStale := httptest.NewRequest("GET", "/monaco/editor.main.css", nil)
	reqStale.Header.Set("If-None-Match", `W/"deadbeef"`)
	recStale := httptest.NewRecorder()
	s.HandleStatic(recStale, reqStale)
	if recStale.Code != http.StatusOK || recStale.Body.Len() == 0 {
		t.Fatalf("stale ETag: status=%d body=%d, want 200 with body", recStale.Code, recStale.Body.Len())
	}
}

// TestHandleStaticIdentityNoGzip verifies the raw bytes are served when the
// client does not accept gzip, and that small/non-gzipable assets (e.g. the
// codicon font) never get Content-Encoding: gzip.
func TestHandleStaticIdentityNoGzip(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest("GET", "/monaco/editor.main.css", nil) // no Accept-Encoding
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("unexpected Content-Encoding %q without gzip accept", rec.Header().Get("Content-Encoding"))
	}
	raw, err := webAssets.ReadFile("web/monaco/editor.main.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatal("identity body does not match embedded asset")
	}

	// Fonts are binary/already-compressed: never gzip even when accepted.
	reqFont := httptest.NewRequest("GET", "/monaco/codicon.ttf", nil)
	reqFont.Header.Set("Accept-Encoding", "gzip")
	recFont := httptest.NewRecorder()
	s.HandleStatic(recFont, reqFont)
	if recFont.Code != http.StatusOK {
		t.Fatalf("font status=%d, want 200", recFont.Code)
	}
	if recFont.Header().Get("Content-Encoding") != "" {
		t.Fatalf("font must not be gzip-encoded, got %q", recFont.Header().Get("Content-Encoding"))
	}
	if recFont.Header().Get("ETag") == "" {
		t.Fatal("expected ETag on font asset")
	}
}

// TestHandleStaticIndexHTMLCached verifies the bootstrap page keeps its
// no-cache policy and is served gzip-cached with 304 revalidation.
func TestHandleStaticIndexHTMLCached(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index Cache-Control=%q, want no-cache", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip index, got %q", rec.Header().Get("Content-Encoding"))
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag on index")
	}

	req304 := httptest.NewRequest("GET", "/", nil)
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	s.HandleStatic(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("index revalidation status=%d, want 304", rec304.Code)
	}
}

// TestHandleStaticMissingAsset verifies unknown paths still 404.
func TestHandleStaticMissingAsset(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/nope/not-here.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func mustGunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}
