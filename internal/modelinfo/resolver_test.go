package modelinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func sampleRegistry() registry {
	return registry{
		"opencode": {
			ID:  "opencode",
			API: "https://opencode.ai/zen/v1",
			Models: map[string]modelEntry{
				"claude-opus-4-8": {
					ID:    "claude-opus-4-8",
					Name:  "Claude Opus 4.8",
					Limit: Limit{Context: 1000000, Output: 128000},
				},
			},
		},
		"opencode-go": {
			ID:  "opencode-go",
			API: "https://opencode.ai/zen/go/v1",
			Models: map[string]modelEntry{
				"mimo-v2.5-pro": {
					ID:    "mimo-v2.5-pro",
					Limit: Limit{Context: 1048576, Output: 128000},
				},
			},
		},
	}
}

func writeRegistry(t *testing.T, path string, reg registry) {
	t.Helper()
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitReady(t *testing.T, r *Resolver, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.RLock()
		ready := r.data != nil && !r.refreshing
		r.mu.RUnlock()
		if ready {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for registry to load")
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for cache file %s", path)
}

func TestResolveContextLimitFromDiskCache(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "models.json")
	writeRegistry(t, cache, sampleRegistry())

	r := NewResolver(cache)
	r.client = &http.Client{Timeout: 50 * time.Millisecond}
	r.url = "http://127.0.0.1:1" // unreachable; must not be needed

	start := time.Now()
	lim, err := r.ResolveContextLimit("https://opencode.ai/zen/v1/", "claude-opus-4-8")
	if err != nil {
		t.Fatalf("ResolveContextLimit: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("disk lookup blocked for %v", time.Since(start))
	}
	if lim.Context != 1000000 {
		t.Fatalf("Context=%d, want 1000000", lim.Context)
	}
}

func TestResolveContextLimitUsesStaleMemoryWithoutBlocking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_ = json.NewEncoder(w).Encode(sampleRegistry())
	}))
	defer srv.Close()

	r := NewResolver("")
	r.url = srv.URL
	r.client = &http.Client{Timeout: 50 * time.Millisecond}
	r.mu.Lock()
	r.setDataLocked(sampleRegistry())
	r.fetchedAt = time.Now().Add(-cacheTTL - time.Second) // force stale
	r.mu.Unlock()

	start := time.Now()
	lim, err := r.ResolveContextLimit("https://opencode.ai/zen/v1", "claude-opus-4-8")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ResolveContextLimit: %v", err)
	}
	if lim.Context != 1000000 {
		t.Fatalf("Context=%d, want 1000000", lim.Context)
	}
	if elapsed > time.Second {
		t.Fatalf("stale lookup blocked for %v; want immediate return", elapsed)
	}
}

func TestWarmThenResolve(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(sampleRegistry())
	}))
	defer srv.Close()

	dir := t.TempDir()
	cache := filepath.Join(dir, "models.json")
	r := NewResolver(cache)
	r.url = srv.URL

	// Cold resolve must not block on network.
	start := time.Now()
	_, err := r.ResolveContextLimit("https://opencode.ai/zen/v1", "claude-opus-4-8")
	if err == nil {
		t.Fatal("expected miss before registry is loaded")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("cold miss blocked for %v", time.Since(start))
	}

	r.Warm()
	waitReady(t, r, 5*time.Second)
	waitFile(t, cache, 5*time.Second)

	lim, err := r.ResolveContextLimit("https://opencode.ai/zen/v1", "claude-opus-4-8")
	if err != nil {
		t.Fatalf("ResolveContextLimit: %v", err)
	}
	if lim.Context != 1000000 || lim.Output != 128000 {
		t.Fatalf("got %+v", lim)
	}

	// Further lookups must be memory-only (no additional fetches).
	warmHits := hits.Load()
	if warmHits < 1 {
		t.Fatal("expected at least one registry fetch to populate cache")
	}
	for i := 0; i < 50; i++ {
		if _, err := r.ResolveContextLimit("https://opencode.ai/zen/go/v1", "mimo-v2.5-pro"); err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != warmHits {
		t.Fatalf("lookups re-fetched registry: hits %d → %d", warmHits, n)
	}
}

func TestResolveContextLimitUnavailableNoCache(t *testing.T) {
	r := NewResolver("")
	r.url = "http://127.0.0.1:1"
	r.client = &http.Client{Timeout: 2 * time.Second} // would hurt if Resolve waited

	start := time.Now()
	_, err := r.ResolveContextLimit("https://opencode.ai/zen/v1", "claude-opus-4-8")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error when registry unavailable and no cache")
	}
	// Must not wait on the HTTP client timeout; allow slow CI scheduling.
	if elapsed > time.Second {
		t.Fatalf("cold miss blocked for %v; must not wait on network", elapsed)
	}
}

func TestProviderID(t *testing.T) {
	r := NewResolver("")
	r.mu.Lock()
	r.setDataLocked(sampleRegistry())
	r.mu.Unlock()

	id, ok := r.ProviderID("https://opencode.ai/zen/v1/")
	if !ok || id != "opencode" {
		t.Fatalf("got %q ok=%v", id, ok)
	}
	if _, ok := r.ProviderID("https://example.com/v1"); ok {
		t.Fatal("expected no match")
	}
}

func TestNormalizeURL(t *testing.T) {
	if got := normalizeURL(" https://x/v1/ "); got != "https://x/v1" {
		t.Fatalf("got %q", got)
	}
}

func TestCachePath(t *testing.T) {
	dir := t.TempDir()
	got := CachePath(dir)
	want := filepath.Join(dir, ".gogen", "models.json")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if CachePath("") != filepath.Join(".", ".gogen", "models.json") {
		t.Fatalf("empty working dir: %q", CachePath(""))
	}
}

func TestSetCachePathClearsMemory(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old", "models.json")
	newPath := filepath.Join(dir, "new", "models.json")

	r := NewResolver(oldPath)
	r.mu.Lock()
	r.setDataLocked(sampleRegistry())
	r.mu.Unlock()

	r.SetCachePath(newPath)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cachePath != newPath {
		t.Fatalf("cachePath=%q, want %q", r.cachePath, newPath)
	}
	if r.data != nil || r.byAPI != nil || !r.fetchedAt.IsZero() {
		t.Fatal("expected in-memory registry cleared for new project path")
	}
}
