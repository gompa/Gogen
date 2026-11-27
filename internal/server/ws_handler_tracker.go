package server

import (
	"sync"
	"time"
)

// wsHandlerDrainWait bounds how long a teardown path waits for the WebSocket
// handler goroutines to unwind. Long enough for a handler to react to the
// closed socket, short enough that a wedged client cannot stall shutdown.
const wsHandlerDrainWait = 2 * time.Second

// wsHandlerTracker counts the live WebSocket handler goroutines (/ws and
// /ws/editor) so shutdown paths and tests can wait for them to drain.
//
// Why this exists: once websocket.Upgrader hijacks the connection, net/http no
// longer tracks the handler goroutine. httptest.Server.Close (and, in
// production, net/http's Shutdown) returns while a read loop is still running
// its last message. Those handlers detach sessions, and a detach can
// orphan-evict a runtime, whose eviction flushes the session store — a write
// that lands after the caller believed the server was torn down. In tests that
// race is the suite's documented "TempDir RemoveAll cleanup: ... directory not
// empty" flake; in production it can outlive the shutdown flush it should have
// been part of.
//
// A counted tracker with a zero-crossing broadcast rather than sync.WaitGroup:
// a handler may be joining (Add) while a waiter is parked at zero, which is the
// case WaitGroup explicitly forbids — the counter accepts it, and a waiter
// woken by one crossing simply parks again on the next one. A sync.Cond would
// need a timed wait it does not provide; the idle channel does (select against
// a timer) and it releases the waiter on the crossing itself instead of on a
// 5 ms polling tick.
type wsHandlerTracker struct {
	mu sync.Mutex
	n  int
	// idle is closed when n reaches 0 and recreated on the 0 -> 1 edge, so a
	// waiter parked on the previous channel is released exactly once by the
	// crossing it captured, while the next generation of handlers gets a fresh
	// channel for the next waiter. Guarded by mu.
	idle chan struct{}
}

// enter records a starting handler. Call it after the upgrade succeeded, so a
// rejected handshake never counts.
//
// The 0 -> 1 edge installs a fresh idle channel — the only allocation this
// tracker makes per drain generation — so the steady state (handlers already
// live) stays allocation-free.
func (t *wsHandlerTracker) enter() {
	t.mu.Lock()
	if t.n == 0 {
		t.idle = make(chan struct{})
	}
	t.n++
	t.mu.Unlock()
}

// leave records a finished handler and releases every waiter parked on the
// current idle channel when — and only when — the count actually reaches 0.
func (t *wsHandlerTracker) leave() {
	t.mu.Lock()
	t.n--
	// n == 0 is the crossing; any other value means handlers are still live
	// and the waiters must stay parked (a straggler must not make them report
	// a drain that did not happen). A nil idle cannot occur on this path — n
	// was > 0, so an enter installed the channel — but the guard keeps an
	// unbalanced leave from panicking on a nil close.
	if t.n == 0 && t.idle != nil {
		close(t.idle)
		t.idle = nil
	}
	t.mu.Unlock()
}

// live reports the handler count. Test-only helper.
func (t *wsHandlerTracker) live() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// wait blocks until no handler is running or the timeout expires, reporting
// whether the tracker drained. Event-based, not polled: it captures the idle
// channel in the same critical section that reads the count
// (capture-then-check), so a leave landing between the two is never missed —
// the captured channel is already closed and the select returns at once — and
// the waiter is released the instant the last handler exits instead of on the
// next 5 ms tick.
//
// Why this is race-free: a waiter that captured one generation's channel
// returns true when that channel is closed, and the channel is closed only by
// the 0 crossing — exactly the drain it was waiting for. A handler entering
// afterwards closes nothing (the 0 -> 1 edge creates a NEW channel), so the
// waiter cannot be released twice and a later waiter parks on the new channel,
// blocking for the next drain instead of inheriting the previous one. The
// close happens only under mu, at the single place n becomes 0, so exactly one
// generation is closed exactly once.
func (t *wsHandlerTracker) wait(timeout time.Duration) bool {
	t.mu.Lock()
	if t.n == 0 {
		t.mu.Unlock()
		return true
	}
	idle := t.idle
	t.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

// WaitWSHandlers blocks until no /ws or /ws/editor handler goroutine is
// running, or timeout expires; it reports whether the handlers drained. Call
// it after closing the connections (srv.Close, closeWSConns): a handler only
// exits once its socket read fails.
//
// ShutdownSessions deliberately does not call this — it is also used by paths
// that keep serving — so a caller that is tearing the server down fully should
// close the connections first and then wait here before its final flush.
func (s *Server) WaitWSHandlers(timeout time.Duration) bool {
	return s.wsLive.wait(timeout)
}
