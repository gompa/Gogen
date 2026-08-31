package streamutil

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// ArgsBatcher coalesces tool-call argument deltas so the consumer is not
// flooded with one rendering event per provider chunk. Deltas are
// accumulated per tool-call index and flushed together on a timer (or at
// explicit boundaries: tool-call start/final, tool execute, round/stream
// end). The send callback is called once per index at flush time with the
// concatenated delta, in index order.
//
// It is the tool-args counterpart of TokenBatcher: the argument stream is
// the highest-rate callback in a tool-heavy turn (a patch_file diff arrives
// as hundreds of ~1KB fragments), and both frontends (TUI Update thread,
// WebSocket frames) need the same coalescing so they stay in sync.
//
// Mirrors TokenBatcher's concurrency contract: the timer goroutine
// (time.AfterFunc) and explicit callers can invoke Flush concurrently, and
// flushMu serializes the grab-and-send phase so a later flush's deltas
// never overtake an earlier one's on the wire.
type ArgsBatcher struct {
	mu   sync.Mutex
	send func(index int, id, name, delta string)
	// pending holds, per tool-call index, the accumulated arg deltas plus
	// the call id/name (they repeat per delta; the last non-empty wins).
	pending map[int]*argsPending
	// flushMu serializes the grab-and-send phase of Flush.
	flushMu  sync.Mutex
	timer    *time.Timer
	interval time.Duration
	closed   bool
}

type argsPending struct {
	id   string
	name string
	// chunks accumulates delta strings without copying; the delta text is
	// joined once at flush time (per-delta string concatenation would copy
	// the whole accumulation every chunk — quadratic on large diffs).
	chunks []string
}

// NewArgsBatcher creates a batcher that flushes every interval.
// The send callback may be called from a different goroutine (the timer).
func NewArgsBatcher(send func(index int, id, name, delta string), interval time.Duration) *ArgsBatcher {
	return &ArgsBatcher{
		send:     send,
		pending:  map[int]*argsPending{},
		interval: interval,
	}
}

// Add enqueues an argument delta for the tool call at index. Safe to call
// concurrently with Flush / Close. After Close, Add is a no-op.
func (b *ArgsBatcher) Add(index int, id, name, delta string) {
	if delta == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	p := b.pending[index]
	if p == nil {
		p = &argsPending{}
		b.pending[index] = p
	}
	if id != "" {
		p.id = id
	}
	if name != "" {
		p.name = name
	}
	p.chunks = append(p.chunks, delta)
	if b.timer == nil {
		b.timer = time.AfterFunc(b.interval, b.Flush)
	}
}

// Flush emits all pending deltas immediately (one send per index, in index
// order). Safe to call concurrently with Add. After Close, Flush is a
// no-op.
func (b *ArgsBatcher) Flush() {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	pending := b.pending
	b.pending = map[int]*argsPending{}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()

	if len(pending) == 0 {
		return
	}
	idxs := make([]int, 0, len(pending))
	for i := range pending {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		p := pending[i]
		if text := strings.Join(p.chunks, ""); text != "" {
			b.send(i, p.id, p.name, text)
		}
	}
}

// Close discards pending deltas, stops the timer, and marks the batcher as
// closed so any late-arriving timer goroutine flush is a no-op. Callers
// that need delivery of pending deltas should call Flush before Close.
func (b *ArgsBatcher) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = nil
	// Mark closed; a concurrent Flush that already grabbed its pending (it
	// holds flushMu, not b.mu) still delivers them — that is the desired
	// "flush before close" ordering, not a post-close append.
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// Reset clears the closed state so the batcher can be reused for a new
// stream round (the adapter calls this on OnStart / OnRoundStart).
func (b *ArgsBatcher) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = false
	if b.pending == nil {
		b.pending = map[int]*argsPending{}
	}
}
