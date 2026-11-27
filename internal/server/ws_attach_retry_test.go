package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// parkOnChange captures the registry's membership channel and returns only
// after the capture happened, so a change the caller triggers next cannot be
// missed (the capture-then-check contract changeChan documents). The released
// channel then reports whether the captured channel was closed within budget.
func parkOnChange(r *sessionRegistry, budget time.Duration) (parked <-chan struct{}, released <-chan bool) {
	parkedCh := make(chan struct{})
	releasedCh := make(chan bool, 1)
	ch := r.changeChan() // capture BEFORE announcing the park
	go func() {
		close(parkedCh)
		timer := time.NewTimer(budget)
		defer timer.Stop()
		select {
		case <-ch:
			releasedCh <- true
		case <-timer.C:
			releasedCh <- false
		}
	}()
	return parkedCh, releasedCh
}

// TestRegistryChangeBroadcast pins the events the /ws handshake's attach retry
// parks on. Every input of first() must broadcast: a removal (the claimed
// session_close finishing its drain), a registration (including the cap
// eviction it performs) and a re-ordering (setDefault). A missing broadcast
// would leave the retry waiting out its 5 s deadline for a change it could
// have handled immediately.
func TestRegistryChangeBroadcast(t *testing.T) {
	t.Run("remove releases a parked waiter", func(t *testing.T) {
		r := newSessionRegistry(8)
		r.register("a", newTestRuntime(t))
		parked, released := parkOnChange(r, 5*time.Second)
		<-parked
		r.remove("a")
		if !<-released {
			t.Fatal("remove did not release the parked waiter")
		}
	})

	t.Run("register releases a parked waiter", func(t *testing.T) {
		r := newSessionRegistry(8)
		r.register("a", newTestRuntime(t))
		parked, released := parkOnChange(r, 5*time.Second)
		<-parked
		r.register("b", newTestRuntime(t))
		if !<-released {
			t.Fatal("register did not release the parked waiter")
		}
	})

	t.Run("cap eviction inside register releases a parked waiter", func(t *testing.T) {
		// register() evicts its victims directly from the map without going
		// through remove(), so it has to broadcast on its own — otherwise the
		// handshake parked on a runtime the cap just evicted would sleep until
		// its deadline.
		r := newSessionRegistry(2)
		r.register("a", newTestRuntime(t)) // the default (front of order): never the victim
		r.register("b", newTestRuntime(t))
		parked, released := parkOnChange(r, 5*time.Second)
		<-parked
		r.register("c", newTestRuntime(t))
		if _, ok := r.get("b"); ok {
			t.Fatal("cap eviction did not evict the oldest idle non-default session")
		}
		if !<-released {
			t.Fatal("cap eviction did not release the parked waiter")
		}
	})

	t.Run("setDefault releases a parked waiter", func(t *testing.T) {
		r := newSessionRegistry(8)
		r.register("a", newTestRuntime(t))
		r.register("b", newTestRuntime(t))
		parked, released := parkOnChange(r, 5*time.Second)
		<-parked
		r.setDefault("b")
		if got := r.first(); got == nil {
			t.Fatal("registry empty after setDefault")
		}
		if !<-released {
			t.Fatal("setDefault did not release the parked waiter")
		}
	})

	t.Run("a change landing before the wait is not missed", func(t *testing.T) {
		r := newSessionRegistry(8)
		r.register("a", newTestRuntime(t))
		ch := r.changeChan()
		r.remove("a") // the change lands between capture and select
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
			t.Fatal("a change that landed before the wait was missed (stale channel)")
		}
	})

	t.Run("a waiter parked after a change blocks until the next one", func(t *testing.T) {
		r := newSessionRegistry(8)
		r.register("a", newTestRuntime(t))
		parked, released := parkOnChange(r, 5*time.Second)
		<-parked
		r.register("b", newTestRuntime(t)) // first change
		if !<-released {
			t.Fatal("the first change did not release the waiter")
		}
		// A later waiter must park on the FRESH channel: the broadcast
		// installs a new one, so it cannot be released by the change that
		// already happened.
		secondParked, secondReleased := parkOnChange(r, 5*time.Second)
		<-secondParked
		select {
		case <-secondReleased:
			t.Fatal("a later waiter was released by an earlier change (shared channel)")
		case <-time.After(30 * time.Millisecond):
		}
		r.remove("b") // second change
		if !<-secondReleased {
			t.Fatal("the second change did not release the second waiter")
		}
	})
}

// handshakeAttachLatency registers a runtime whose session_close has been
// claimed (the drain window: closed to new clients but still registered),
// parks a real /ws handshake on it, unregisters it after parkDelay and returns
// how long the handshake took to ATTACH from that removal. The clock starts
// when the runtime is unregistered and stops when the client receives the
// session_state frame the attach writes — gorilla's Dial returns at the HTTP
// 101, which the upgrade sends before the pane is resolved, so measuring the
// Dial would measure the wrong thing.
func handshakeAttachLatency(t *testing.T, s *Server, srvURL string, id string, parkDelay time.Duration) time.Duration {
	t.Helper()
	rt := newTestRuntime(t)
	s.registry.register(id, rt)
	s.registry.setDefault(id) // make it the pane the handshake resolves
	if got := s.registry.first(); got != rt {
		t.Fatalf("first() = %v, want the registered runtime", got)
	}
	if !rt.claimClose() {
		t.Fatal("claimClose refused a clientless runtime")
	}

	type dialResult struct {
		conn *websocket.Conn
		err  error
		at   time.Time
	}
	dialed := make(chan dialResult, 1)
	go func() {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srvURL, "http")+"/ws", nil)
		if err != nil {
			dialed <- dialResult{err: err, at: time.Now()}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var first WSMessage // session_state: written by the successful attach
		err = conn.ReadJSON(&first)
		dialed <- dialResult{conn: conn, err: err, at: time.Now()}
	}()

	// The handler counts itself before it resolves the pane, so a live count
	// with no client attached means it reached the retry.
	waitForBounded(time.Second, func() bool { return s.wsLive.live() == 1 })
	if !waitForBounded(time.Second, func() bool { return rt.clientCount() == 0 }) {
		t.Fatal("the handshake attached to a runtime whose close was claimed")
	}
	time.Sleep(parkDelay)

	removedAt := time.Now()
	s.registry.remove(id) // the claimed runtime's eviction finishing
	res := <-dialed
	if res.err != nil {
		t.Fatalf("dial /ws: %v", res.err)
	}
	latency := res.at.Sub(removedAt)
	if latency < 0 {
		res.conn.Close()
		t.Fatalf("the handshake attached %s before the claimed runtime was unregistered; the scenario did not park the retry", -latency)
	}
	// Close first: the handler only exits once its socket read fails.
	res.conn.Close()
	if !s.WaitWSHandlers(5 * time.Second) {
		t.Fatalf("handlers did not drain after the socket closed (live = %d)", s.wsLive.live())
	}
	return latency
}

// TestWSHandshakeAttachesWhenClaimedRuntimeLeaves is the end-to-end proof for
// the attach retry: it parks a real /ws handshake on a runtime whose
// session_close has been claimed and measures how long the attach takes once
// that runtime is unregistered. The old retry re-probed every 25 ms, so it
// answered on its next tick (0-25 ms later); the membership broadcast wakes it
// immediately. The park delays shift the removal across a would-be tick
// window, so no single lucky alignment can pass a polled retry.
func TestWSHandshakeAttachesWhenClaimedRuntimeLeaves(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)

	for i, park := range []time.Duration{10 * time.Millisecond, 23 * time.Millisecond, 37 * time.Millisecond, 41 * time.Millisecond, 48 * time.Millisecond} {
		name := fmt.Sprintf("removal parked %s after the refusal", park)
		t.Run(name, func(t *testing.T) {
			id := fmt.Sprintf("claimed-handshake-%d", i)
			latency := handshakeAttachLatency(t, s, srv.URL, id, park)
			t.Logf("attach latency after the claimed runtime left: %s", latency)
			if latency > 8*time.Millisecond {
				t.Fatalf("handshake took %s to attach after the claimed runtime left, want a wake-up (the old loop slept 25ms)", latency)
			}
		})
	}
}

// TestWSHandshakeNoSessionAvailableWhenEveryCandidateStaysClaimed pins the
// deadline half of the retry: with the only resolvable runtime claimed and
// never unregistered, the handshake parks on the membership broadcast until
// its 5 s budget expires and then fails with the same error frame a missing
// session produces (the client's reconnect logic retries). This is the
// guarantee the polled loop also gave — same deadline, same frame — and the
// proof that parking on an event cannot hang the handshake forever.
func TestWSHandshakeNoSessionAvailableWhenEveryCandidateStaysClaimed(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)

	rt := newTestRuntime(t)
	const id = "claimed-forever"
	s.registry.register(id, rt)
	s.registry.setDefault(id) // first() stays on this runtime for the whole wait
	if !rt.claimClose() {
		t.Fatal("claimClose refused a clientless runtime")
	}
	t.Cleanup(func() { s.registry.remove(id) })

	start := time.Now()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var msg WSMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read handshake error frame: %v", err)
	}
	elapsed := time.Since(start)
	if msg.Type != "response" || msg.Content != "Error: no session available" {
		t.Fatalf("handshake frame = %+v, want the 'no session available' response", msg)
	}
	if elapsed < 5*time.Second {
		t.Fatalf("handshake failed after %s, want the full 5s retry budget", elapsed)
	}
}
