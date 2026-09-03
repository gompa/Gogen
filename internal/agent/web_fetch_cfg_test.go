package agent

import (
	"slices"
	"testing"
)

// TestWebCfgFetchSettingsGateOnFetchOnly pins the per-field fallback for the
// fetch-specific settings (mode, allowed domains). Configuring only web_search
// must not make mode()/allowedDomains() read the (empty) configured fields;
// they must fall back to env, exactly like isFetchOn/isSearchOn do per flag.
//
// Regression: these accessors previously gated on
// `fetchOn != nil || searchOn != nil`, so configuring only search silently
// switched them off the env fallback.
func TestWebCfgFetchSettingsGateOnFetchOnly(t *testing.T) {
	// Save and restore the global web config so the test is isolated from
	// other tests that mutate it.
	webCfg.mu.Lock()
	oldFetchOn, oldSearchOn := webCfg.fetchOn, webCfg.searchOn
	oldMode, oldDomains := webCfg.fetchMode, webCfg.fetchDomains
	webCfg.mu.Unlock()
	t.Cleanup(func() {
		webCfg.mu.Lock()
		webCfg.fetchOn, webCfg.searchOn = oldFetchOn, oldSearchOn
		webCfg.fetchMode, webCfg.fetchDomains = oldMode, oldDomains
		webCfg.mu.Unlock()
	})

	on := true
	off := false

	t.Run("search-only falls back to env", func(t *testing.T) {
		webCfg.mu.Lock()
		webCfg.fetchOn = nil
		webCfg.searchOn = &on
		webCfg.fetchMode = ""
		webCfg.fetchDomains = nil
		webCfg.mu.Unlock()

		if got := webCfg.mode(); got != envFetchMode() {
			t.Errorf("mode() = %q, want envFetchMode() = %q", got, envFetchMode())
		}
		if got, want := webCfg.allowedDomains(), envAllowedDomains(); !slices.Equal(got, want) {
			t.Errorf("allowedDomains() = %v, want envAllowedDomains() = %v", got, want)
		}
		// Sanity: the configured fields are empty, so the env fallback is what
		// distinguishes the fixed behavior from the old gate.
		if got := webCfg.mode(); got == "" {
			t.Fatalf("mode() returned empty configured value instead of env fallback")
		}
	})

	t.Run("fetch-configured wins over env", func(t *testing.T) {
		webCfg.mu.Lock()
		webCfg.fetchOn = &on
		webCfg.searchOn = &off
		webCfg.fetchMode = "all"
		webCfg.fetchDomains = []string{"example.com"}
		webCfg.mu.Unlock()

		if got := webCfg.mode(); got != "all" {
			t.Errorf("mode() = %q, want %q", got, "all")
		}
		if got := webCfg.allowedDomains(); !slices.Equal(got, []string{"example.com"}) {
			t.Errorf("allowedDomains() = %v, want [example.com]", got)
		}
	})
}
