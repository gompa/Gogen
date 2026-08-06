package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

var defaultAllowedHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
}

func parseAllowedOrigins(raw string) map[string]struct{} {
	// Entries are stored as bare hostnames: checkWSOrigin compares
	// u.Hostname() (which strips brackets) and parseAllowedOrigins trims
	// brackets below, so a bracketed "[::1]" key could never match — the
	// bare "::1" form is the only one that works.
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
		// checkWSOrigin compares Hostname() (no port), so store hostnames —
		// not host:port strings — or entries like "example.com:8080" would
		// silently never match. Handle both the schemed form (url.Parse) and
		// a bare "host:port" (which parses as an opaque URL with no Host).
		host := ""
		if u, err := url.Parse(part); err == nil && u.Host != "" {
			host = u.Hostname()
		} else if h, _, err := net.SplitHostPort(part); err == nil && h != "" {
			host = h
		}
		if host == "" {
			host = strings.TrimPrefix(part, ".")
		}
		// IPv6 entries may carry brackets ("[::1]") — Hostname() strips them,
		// so store the bare form for a consistent lookup.
		out[strings.ToLower(strings.Trim(host, "[]"))] = struct{}{}
	}
	return out
}

func checkWSOrigin(r *http.Request, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		allowed = defaultAllowedHosts
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	// Host header may carry a port ("localhost:8081") and, for IPv6, brackets
	// ("[::1]:8081"). Splitting on ":" alone turns "[::1]:8081" into "[" and
	// broke the loopback / same-host checks for IPv6 clients. SplitHostPort
	// handles both; trim brackets for the comparison.
	reqHost := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		reqHost = h
	}
	reqHost = strings.ToLower(strings.Trim(reqHost, "[]"))
	loopback := reqHost == "localhost" || reqHost == "127.0.0.1" || reqHost == "::1"
	if origin == "" {
		// Non-browser clients (curl, etc.) omit Origin. Only allow that on loopback.
		return loopback
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
	return host == reqHost
}
