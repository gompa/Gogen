package server

import (
	"fmt"
	"log"
	"runtime/debug"

	"gogen/internal/debuglog"
)

// recoverGoroutine is the panic guard for server goroutines that run turn,
// tool, probe, or parse code: `defer recoverGoroutine("provider test")`.
//
// net/http recovers panics raised on the handler goroutine only. A panic in a
// goroutine the handler spawned — a turn, a compaction, a child agent turn, a
// provider/MCP probe, an index rebuild — is fatal to the whole process, taking
// every live session with it. Those goroutines therefore recover here and log
// instead.
//
// It must be the OUTERMOST defer (registered first, so it runs LAST): a panic
// still unwinds the goroutine's other defers, so the turn release, the stream
// handle clear, the turn_end broadcast, and the orphan check all run before
// this recovers. Registered last instead, it would fire before them and a
// panicking turn would deadlock the session on a turn lock nobody releases.
//
// Recovery is deliberately logging-only. A turn's error frame is written
// inside the turn body, before turn_end; adding a second frame here would be a
// protocol change (the panic can land after the success frame was written).
// The turn_end broadcast from the turn body's own defers already leaves the
// client's busy state, so the UI converges even though the panic is only
// visible in the log.
func recoverGoroutine(where string) {
	if r := recover(); r != nil {
		logRecoveredPanic(where, r)
	}
}

// logRecoveredPanic reports a recovered panic with its stack. Callers that
// have a result channel to answer (a child turn) use this directly so the
// waiter is released instead of blocking forever.
func logRecoveredPanic(where string, r any) {
	stack := debug.Stack()
	log.Printf("panic in %s (recovered, session stays alive): %v\n%s", where, r, stack)
	debuglog.Write("server/panic", "recovered panic in spawned goroutine", "goroutine-panic", map[string]any{
		"where": where,
		"panic": fmt.Sprint(r),
		"stack": string(stack),
	})
}
