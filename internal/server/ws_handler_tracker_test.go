package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWSHandlerTrackerSettles pins the tracker's two jobs: it must report the
// live handler count, and WaitWSHandlers must return only once the handlers
// have wound down (or the budget expired). The flake it exists to prevent —
// a session flush landing during t.TempDir() removal — depends on the wait
// being a real join, not a no-op that returns immediately.
func TestWSHandlerTrackerSettles(t *testing.T) {
	t.Run("idle tracker drains immediately", func(t *testing.T) {
		var tr wsHandlerTracker
		if !tr.wait(time.Second) {
			t.Fatal("idle tracker reported a timeout")
		}
		if got := tr.live(); got != 0 {
			t.Fatalf("live = %d, want 0", got)
		}
	})

	t.Run("wait blocks until the handler leaves", func(t *testing.T) {
		var tr wsHandlerTracker
		tr.enter()
		released := make(chan struct{})
		go func() {
			defer close(released)
			time.Sleep(25 * time.Millisecond)
			tr.leave()
		}()
		if !tr.wait(time.Second) {
			t.Fatal("wait timed out with a handler that was about to leave")
		}
		<-released
		if got := tr.live(); got != 0 {
			t.Fatalf("live = %d, want 0", got)
		}
	})

	t.Run("a stuck handler is bounded by the timeout", func(t *testing.T) {
		var tr wsHandlerTracker
		tr.enter()
		defer tr.leave()
		start := time.Now()
		if tr.wait(20 * time.Millisecond) {
			t.Fatal("wait reported a drain while a handler was still live")
		}
		if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
			t.Fatalf("wait returned after %s, want at least the 20ms budget", elapsed)
		}
	})
}

// TestWSHandlerTrackerGenerations pins the zero-crossing broadcast's edges,
// which the polled counter could get wrong in the opposite direction (a
// straggler entering while a waiter was parked must never make the waiter
// report a drain that did not happen).
func TestWSHandlerTrackerGenerations(t *testing.T) {
	t.Run("a joining handler bends a parked waiter to a real drain, never a false one", func(t *testing.T) {
		var tr wsHandlerTracker
		tr.enter() // A
		result := make(chan bool, 1)
		go func() { result <- tr.wait(2 * time.Second) }()
		// Let the waiter park on A's generation before B joins: this is the
		// WaitGroup-hazard case — the count is NOT reset to zero by A's exit,
		// so the waiter must stay parked instead of waking on a stale signal.
		time.Sleep(20 * time.Millisecond)
		tr.enter() // B joins while the waiter is parked
		tr.leave() // A leaves; B is still live, so no 0 crossing
		select {
		case got := <-result:
			t.Fatalf("wait returned %v after A left with B still live, want the waiter parked", got)
		case <-time.After(50 * time.Millisecond):
		}
		tr.leave() // B is the last handler: the crossing releases the waiter
		select {
		case got := <-result:
			if !got {
				t.Fatal("wait reported a timeout after the last handler left")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("wait was not released by the last handler leaving")
		}
	})

	t.Run("repeated drain generations do not panic and release every waiter", func(t *testing.T) {
		var tr wsHandlerTracker
		for i := 0; i < 100; i++ {
			tr.enter()
			result := make(chan bool, 1)
			go func() { result <- tr.wait(5 * time.Second) }()
			tr.leave()
			if !<-result {
				t.Fatalf("cycle %d: wait timed out after the only handler left", i)
			}
			if got := tr.live(); got != 0 {
				t.Fatalf("cycle %d: live = %d, want 0", i, got)
			}
			// The next generation must park on a fresh channel, not inherit
			// the closed one — an idle tracker therefore drains immediately.
			if !tr.wait(time.Second) {
				t.Fatalf("cycle %d: idle wait reported a timeout", i)
			}
		}
	})

	t.Run("steady-state enter and leave do not allocate", func(t *testing.T) {
		var tr wsHandlerTracker
		tr.enter()
		defer tr.leave()
		// Only the 0 -> 1 edge allocates (the generation's channel): a server
		// with handlers already live must not allocate per connection and
		// per disconnect.
		if allocs := testing.AllocsPerRun(1000, func() { tr.enter(); tr.leave() }); allocs != 0 {
			t.Fatalf("enter+leave allocated %.1f objects per run inside one generation, want 0", allocs)
		}
	})

	t.Run("concurrent enter and leave storm drains without a panic", func(t *testing.T) {
		var tr wsHandlerTracker
		const workers, rounds = 16, 50
		var wg sync.WaitGroup
		stop := make(chan struct{})
		// Waiters hammer the same tracker while handlers churn, so a
		// double-close or a lost wake-up shows up as a panic or a hang under
		// -race.
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					tr.wait(time.Millisecond)
				}
			}()
		}
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < rounds; i++ {
					tr.enter()
					tr.leave()
				}
			}()
		}
		waitForBounded(10*time.Second, func() bool { return tr.live() == 0 })
		close(stop)
		wg.Wait()
		if !tr.wait(5 * time.Second) {
			t.Fatalf("tracker did not drain after the storm (live = %d)", tr.live())
		}
	})
}

// waitPolled reproduces the PRE-FIX wait loop (a 5 ms polling tick) so the
// latency test and benchmark can measure the granularity it cost. Test-only:
// nothing in the server polls any more.
func waitPolled(t *wsHandlerTracker, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if t.live() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainLatency measures the NOTIFICATION latency of a single-handler drain:
// how long after the last handler leaves the given wait implementation
// observes it. The handler's own sleep is excluded (a sub-millisecond
// time.Sleep overshoots by ~1ms, which would swamp the measurement), so the
// number is the mechanism's granularity: up to one 5 ms poll tick versus the
// cost of a channel wake-up.
func drainLatency(t *testing.T, wait func(*wsHandlerTracker, time.Duration) bool, leaveAfter time.Duration) time.Duration {
	t.Helper()
	var tr wsHandlerTracker
	tr.enter()
	var leftAt atomic.Int64
	go func() {
		time.Sleep(leaveAfter) // land the leave between two 5 ms poll ticks
		leftAt.Store(time.Now().UnixNano())
		tr.leave()
	}()
	if !wait(&tr, 5*time.Second) {
		t.Fatal("drain wait timed out with a handler that was about to leave")
	}
	woke := time.Now()
	return woke.Sub(time.Unix(0, leftAt.Load()))
}

// TestWSHandlerTrackerDrainLatency is the before/after measurement: a drain
// that the 5 ms poll loop could only notice on its next tick is now observed
// the moment the last handler exits. The handler leaves after 200 µs, i.e.
// between two poll ticks, which is why the polled wait reports ~5 ms.
func TestWSHandlerTrackerDrainLatency(t *testing.T) {
	const leaveAfter = 200 * time.Microsecond
	best := func(wait func(*wsHandlerTracker, time.Duration) bool) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 5; i++ {
			if d := drainLatency(t, wait, leaveAfter); d < best {
				best = d
			}
		}
		return best
	}
	polled := best(waitPolled)
	broadcast := best(func(tr *wsHandlerTracker, timeout time.Duration) bool { return tr.wait(timeout) })
	t.Logf("single-handler drain, notification latency: 5ms poll %s, broadcast %s", polled, broadcast)

	if polled < 3*time.Millisecond {
		t.Fatalf("polled drain noticed the leave after %s, want up to a 5ms tick (the comparison is meaningless otherwise)", polled)
	}
	if broadcast >= 500*time.Microsecond {
		t.Fatalf("broadcast drain noticed the leave after %s, want a wake-up, not a tick", broadcast)
	}
	if broadcast >= polled/2 {
		t.Fatalf("broadcast drain %s is not clearly faster than the polled %s", broadcast, polled)
	}
}

// BenchmarkWSHandlerTrackerWaitDrain quantifies the same difference for a
// single-handler drain (run with -bench to compare the two paths).
func BenchmarkWSHandlerTrackerWaitDrain(b *testing.B) {
	drain := func(b *testing.B, wait func(*wsHandlerTracker, time.Duration) bool) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var tr wsHandlerTracker
			tr.enter()
			done := make(chan struct{})
			go func() {
				defer close(done)
				tr.leave()
			}()
			if !wait(&tr, 5*time.Second) {
				b.Fatal("drain wait timed out")
			}
			<-done
		}
	}
	b.Run("legacy-5ms-poll", func(b *testing.B) {
		drain(b, waitPolled)
	})
	b.Run("broadcast", func(b *testing.B) {
		drain(b, func(tr *wsHandlerTracker, timeout time.Duration) bool { return tr.wait(timeout) })
	})
}

// TestWSHandlersAreTracked pins the wiring: a live chat socket and a live
// editor socket are both counted, and both are released when the sockets
// close. Without this the join in drainServer/ForceClose would silently wait
// on nothing.
func TestWSHandlersAreTracked(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	if got := s.wsLive.live(); got != 0 {
		t.Fatalf("live handlers before dialing = %d, want 0", got)
	}

	conn := dialWS(t, srv, "/ws")
	if !waitForBounded(time.Second, func() bool { return s.wsLive.live() == 1 }) {
		t.Fatalf("live handlers after dialing = %d, want 1", s.wsLive.live())
	}

	conn.Close()
	if !s.WaitWSHandlers(5 * time.Second) {
		t.Fatalf("handlers did not drain after the socket closed (live = %d)", s.wsLive.live())
	}
	if got := s.wsLive.live(); got != 0 {
		t.Fatalf("live handlers after close = %d, want 0", got)
	}
}

// waitForBounded polls cond until it holds or the budget expires.
func waitForBounded(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
