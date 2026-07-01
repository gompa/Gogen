package server

import (
	"net/http"
	"net/url"
	"strings"
)

var defaultAllowedHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
	"[::1]":     {},
}

func parseAllowedOrigins(raw string) map[string]struct{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		out := make(map[string]struct{}, len(defaultAllowedHosts))
		for k, v := range defaultAllowedHosts {
			out[k] = v
		}
		return out
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if u, err := url.Parse(part); err == nil && u.Host != "" {
			out[strings.ToLower(u.Hostname())] = struct{}{}
			continue
		}
		out[strings.ToLower(strings.TrimPrefix(part, "."))] = struct{}{}
	}
	return out
}

func checkWSOrigin(r *http.Request, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		allowed = defaultAllowedHosts
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := allowed[host]; ok {
		return true
	}
	// Allow the UI served from the same host (Origin host:port vs Host header).
	reqHost := strings.ToLower(strings.Split(r.Host, ":")[0])
	return host == reqHost
}
