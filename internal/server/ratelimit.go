package server

import (
	"net"
	"net/http"
	"sync"

	"golang.org/x/time/rate"
)

const (
	defaultMaxWSConns    = 32
	defaultWSMsgRate     = 10 // messages per second per connection
	defaultWSMsgBurst    = 20
	authCookieName       = "gogen_web_token"
	authCookieMaxAgeSecs = 7 * 24 * 60 * 60
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

func newWSMessageLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Limit(defaultWSMsgRate), defaultWSMsgBurst)
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
	rate     rate.Limit
	burst    int
}

func newIPLimiter(perSec float64, burst int) *ipLimiter {
	return &ipLimiter{
		limiters: make(map[string]*rate.Limiter),
		rate:     rate.Limit(perSec),
		burst:    burst,
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[ip]
	if !ok {
		lim = rate.NewLimiter(l.rate, l.burst)
		l.limiters[ip] = lim
		// Opportunistic prune to avoid unbounded growth.
		if len(l.limiters) > 10_000 {
			l.limiters = map[string]*rate.Limiter{ip: lim}
		}
	}
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
