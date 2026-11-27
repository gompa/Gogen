package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUpgradeFailureWithoutConnLimiter pins the failed-handshake cleanup: the
// connection slot is only taken when a limiter exists, so release is nil on a
// server built without one (a nil Server in a test, or one whose limiter was
// never installed). Calling it unconditionally panicked on ANY refused
// handshake — a plain GET on the socket, a non-websocket client — instead of
// answering the HTTP error the upgrade had already written.
func TestUpgradeFailureWithoutConnLimiter(t *testing.T) {
	s := &Server{} // no connLimiter: release stays nil
	if s.connLimiter != nil {
		t.Fatal("setup: the fixture must have no connection limiter")
	}

	// A request without the websocket upgrade headers is refused by the
	// upgrader itself, which is the path that runs the release.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	rec := httptest.NewRecorder()
	got, err := s.upgradeWS(rec, req)
	if err == nil || got != nil {
		t.Fatalf("upgradeWS(non-websocket request) = (%v, %v), want (nil, %v)", got, err, errWSUpgrade)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upgrader's %d", rec.Code, http.StatusBadRequest)
	}
}

// TestUpgradeFailureWithConnLimiterReleases pins the other half: when a
// limiter DID hand out a slot, a failed handshake returns it (a leak would
// shrink the connection budget until the server refuses every tab).
func TestUpgradeFailureWithConnLimiterReleases(t *testing.T) {
	s := &Server{connLimiter: newRateLimitState(defaultMaxWSConns)}
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if _, err := s.upgradeWS(httptest.NewRecorder(), req); err != errWSUpgrade {
		t.Fatalf("upgradeWS error = %v, want %v", err, errWSUpgrade)
	}
	s.connLimiter.mu.Lock()
	held := s.connLimiter.conns
	s.connLimiter.mu.Unlock()
	if held != 0 {
		t.Fatalf("connections held = %d after a failed handshake, want 0", held)
	}
}
