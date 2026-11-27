package server

import (
	"sync"
	"testing"
)

// TestRecoverGoroutineRunsAfterCleanupDefers pins the ordering contract that
// makes the guard safe: registered as the FIRST defer of a goroutine it runs
// LAST, i.e. after every cleanup defer the goroutine registered. Were it
// registered last instead, a panicking turn would recover before its
// turn-release/stream-end defers ran and leave the session parked on a turn
// lock nobody releases — worse than the crash it guards against.
func TestRecoverGoroutineRunsAfterCleanupDefers(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	done := make(chan struct{})

	go func() {
		defer close(done)
		// The guard is registered first…
		defer recoverGoroutine("ordering test")
		// …so this cleanup, registered after it, runs before the recovery.
		defer record("cleanup")
		panic("boom")
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "cleanup" {
		t.Fatalf("defer order = %v, want [cleanup] before the panic is recovered", order)
	}
}

// TestRecoverGoroutineIsNoOpWithoutPanic keeps the guard from changing the
// ordinary path: it must not log, unwind, or otherwise disturb a goroutine
// that returns normally (the recover() call simply yields nil).
func TestRecoverGoroutineIsNoOpWithoutPanic(t *testing.T) {
	ran := false
	func() {
		defer recoverGoroutine("ordering test")
		ran = true
	}()
	if !ran {
		t.Fatal("the guarded function did not run to completion")
	}
}
