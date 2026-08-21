package server

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultMaxWSConns = 32
	authCookieName    = "gogen_web_token"
	// authCookieMaxAgeSecs is the session cookie lifetime. The auth token is
	// persisted across restarts for non-loopback binds, so the cookie can
	// outlive a single server run (30 days); a re-login via the printed
	// link/QR mints a fresh cookie.
	authCookieMaxAgeSecs = 30 * 24 * 60 * 60
)

type rateLimitState struct {
	mu       sync.Mutex
	conns    int
	maxConns int
}

func newRateLimitState(maxConns int) *rateLimitState {
	if maxConns <= 0 {
		maxConns = defaultMaxWSConns
	}
	return &rateLimitState{maxConns: maxConns}
}

func (r *rateLimitState) acquireConn() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns >= r.maxConns {
		return false
	}
	r.conns++
	return true
}

func (r *rateLimitState) releaseConn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns > 0 {
		r.conns--
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipLimiter tracks per-IP connection attempt rates for HTTP/WS upgrades.
type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	lastUsed map[string]time.Time
	rate     rate.Limit
	burst    int
}

func newIPLimiter(perSec float64, burst int) *ipLimiter {
	return &ipLimiter{
		limiters: make(map[string]*rate.Limiter),
		lastUsed: make(map[string]time.Time),
		rate:     rate.Limit(perSec),
		burst:    burst,
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	lim, ok := l.limiters[ip]
	if !ok {
		lim = rate.NewLimiter(l.rate, l.burst)
		l.limiters[ip] = lim
		// Opportunistic prune to avoid unbounded growth.
		if len(l.limiters) > 10_000 {
			// Evict the least-recently-used entry: a random eviction (map
			// iteration order) can drop a limiter that is actively
			// throttling an IP, granting it a fresh burst. The scan is
			// O(map size) but only runs once per new IP over the cap.
			var oldest string
			var oldestTime time.Time
			for k, t := range l.lastUsed {
				if oldest == "" || t.Before(oldestTime) {
					oldest, oldestTime = k, t
				}
			}
			if oldest != "" {
				delete(l.limiters, oldest)
				delete(l.lastUsed, oldest)
			}
		}
	}
	l.lastUsed[ip] = now
	return lim.Allow()
}

func setAuthCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   authCookieMaxAgeSecs,
	})
}
