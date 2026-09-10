package session

import (
	"fmt"
	"testing"
	"time"
)

// seedListCache installs n entries with the given age under keys prefixed with
// prefix, returning the keys (for key-scoped cleanup: other tests in this
// package use the same package-level cache, keyed by their own temp dirs).
func seedListCache(prefix string, n int, age time.Duration) []string {
	keys := make([]string, 0, n)
	listCacheMu.Lock()
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("%s-%d", prefix, i)
		listCache[k] = listCacheEntry{time: time.Now().Add(-age)}
		keys = append(keys, k)
	}
	listCacheMu.Unlock()
	return keys
}

func clearListCacheKeys(keys ...string) {
	listCacheMu.Lock()
	for _, k := range keys {
		delete(listCache, k)
	}
	listCacheMu.Unlock()
}

// TestCacheListSweepsExpiredEntries pins that cacheList bounds the package-level
// listing cache once it reaches the sweep threshold: an entry past the 1s TTL
// can never be served (the read path requires age < 1s), so it is dropped
// instead of being retained for the life of the process — the working-dir
// switch creates one entry per directory visited.
func TestCacheListSweepsExpiredEntries(t *testing.T) {
	stale := seedListCache("/cache-sweep-stale", listCacheSweepThreshold, time.Hour)
	t.Cleanup(func() { clearListCacheKeys(append(stale, "/cache-sweep-live")...) })

	cacheList("/cache-sweep-live", []SessionInfo{{ID: "live"}})

	listCacheMu.RLock()
	defer listCacheMu.RUnlock()
	for _, k := range stale {
		if _, ok := listCache[k]; ok {
			t.Fatalf("expired entry %s survived the sweep", k)
		}
	}
	ce, ok := listCache["/cache-sweep-live"]
	if !ok {
		t.Fatal("freshly cached entry was swept")
	}
	if len(ce.info) != 1 || ce.info[0].ID != "live" {
		t.Fatalf("cached listing = %+v, want the live session", ce.info)
	}
}

// TestCacheListKeepsFreshEntries pins the amortization the sweep relies on:
// fresh entries (including expired-TTL-deleted survivors from a concurrent
// caller) are never swept, so a process that stays on one working directory
// pays nothing and recently cached listings stay hot.
func TestCacheListKeepsFreshEntries(t *testing.T) {
	fresh := seedListCache("/cache-fresh", 2, 0)
	t.Cleanup(func() { clearListCacheKeys(append(fresh, "/cache-fresh-new")...) })

	cacheList("/cache-fresh-new", []SessionInfo{{ID: "new"}})

	listCacheMu.RLock()
	defer listCacheMu.RUnlock()
	for _, k := range fresh {
		if _, ok := listCache[k]; !ok {
			t.Fatalf("fresh entry %s was swept", k)
		}
	}
	if _, ok := listCache["/cache-fresh-new"]; !ok {
		t.Fatal("newly cached entry missing")
	}
}
