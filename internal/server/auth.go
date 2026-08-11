package server

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

func (s *Server) checkAuth(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		if tokenMatches(strings.TrimSpace(c.Value), s.authToken) {
			return true
		}
	}
	if tok := strings.TrimSpace(r.Header.Get("X-Gogen-Token")); tokenMatches(tok, s.authToken) {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		tok := strings.TrimSpace(auth[7:])
		if tokenMatches(tok, s.authToken) {
			return true
		}
	}
	return false
}

// tokenMatches compares a candidate token against the expected one in
// constant time so the auth check does not leak timing information about
// the token value. An empty candidate never matches.
func tokenMatches(candidate, expected string) bool {
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func IsLoopbackBind(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	} else if strings.HasPrefix(addr, ":") {
		host = "0.0.0.0"
	}
	host = strings.TrimSpace(strings.ToLower(host))
	// Empty host in ":port" form means all interfaces.
	if host == "" {
		return false
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		ip := net.ParseIP(strings.Trim(host, "[]"))
		return ip != nil && ip.IsLoopback()
	}
}
