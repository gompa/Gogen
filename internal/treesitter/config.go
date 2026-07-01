package treesitter

import (
	"os"
	"strings"
	"sync"
)

var (
	cfgMu       sync.RWMutex
	cfgEnabled  *bool
	cfgLangs    string
	cfgLangsSet bool
)

// Configure applies runtime tree-sitter settings from merged config.
func Configure(enabled bool, langs string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgEnabled = &enabled
	cfgLangs = langs
	cfgLangsSet = true
}

// Enabled reports whether tree-sitter syntax checks are active.
func Enabled() bool {
	cfgMu.RLock()
	if cfgEnabled != nil {
		e := *cfgEnabled
		cfgMu.RUnlock()
		return e
	}
	cfgMu.RUnlock()
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GOGEN_TREESITTER")))
	if v == "off" || v == "0" || v == "false" {
		return false
	}
	return true
}

func allowedLangs() map[string]struct{} {
	cfgMu.RLock()
	var raw string
	if cfgLangsSet {
		raw = strings.TrimSpace(cfgLangs)
		cfgMu.RUnlock()
	} else {
		cfgMu.RUnlock()
		raw = strings.TrimSpace(os.Getenv("GOGEN_TREESITTER_LANGS"))
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" {
			out[name] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func langAllowed(name string, allowed map[string]struct{}) bool {
	if allowed == nil {
		return true
	}
	_, ok := allowed[strings.ToLower(name)]
	return ok
}
