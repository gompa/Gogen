// Package streamutil provides shared utilities for streaming token output
// across the TUI and WebSocket frontends.
package streamutil

import (
	"strings"
	"sync"
	"time"
)

// TokenBatcher coalesces stream/thinking tokens so the consumer channel
// is not flooded with one message per token. Tokens are grouped into
// adjacent same-kind segments (content vs thinking) and flushed together
// on a timer. The send callback is called for each segment at flush time.
//
// The TUI and server WebSocket both use the same batching logic with
// different send formats; this shared type keeps them in sync.
type TokenBatcher struct {
	mu   sync.Mutex
	send func(think bool, text string)
	segs []seg
	// flushMu serializes the grab-and-send phase of Flush. Flush can be
	// invoked concurrently — the timer goroutine (time.AfterFunc) and a
	// caller (OnStreamEnd / tool boundaries) — and without this lock the two
	// drains would call send from two goroutines, letting a later flush's
	// segments overtake an earlier one's on the wire.
	flushMu  sync.Mutex
	timer    *time.Timer
	interval time.Duration
	closed   bool
}

type seg struct {
	think bool
	// chunks accumulates token strings without copying; the segment text is
	// joined once at flush time. Appending to a single string (text += token)
	// would copy the whole accumulated segment per token — quadratic under
	// backpressure when a stalled send callback lets a segment grow large.
	chunks []string
}

// NewTokenBatcher creates a batcher that flushes every interval.
// The send callback receives (think, text) for each coalesced segment.
// It may be called from a different goroutine (the timer callback).
func NewTokenBatcher(send func(think bool, text string), interval time.Duration) *TokenBatcher {
	return &TokenBatcher{
		send:     send,
		interval: interval,
	}
}

// StreamToken enqueues a content token for batching.
func (b *TokenBatcher) StreamToken(text string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(false, text)
}

// ThinkToken enqueues a thinking/reasoning token for batching.
func (b *TokenBatcher) ThinkToken(text string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(true, text)
}

// Flush emits all pending segments immediately. Safe to call concurrently
// with StreamToken / ThinkToken. After Close, Flush is a no-op.
func (b *TokenBatcher) Flush() {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	segs := b.segs
	b.segs = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()

	for _, s := range segs {
		text := strings.Join(s.chunks, "")
		if text == "" {
			continue
		}
		b.send(s.think, text)
	}
}

// Close discards pending segments, stops the timer, and marks the batcher
// as closed so any late-arriving timer goroutine flush is a no-op.
// Callers that need delivery of pending tokens should call Flush before Close.
func (b *TokenBatcher) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.segs = b.segs[:0]
	// Mark closed; a concurrent Flush that already grabbed its segments (it
	// holds flushMu, not b.mu) still delivers them — that is the desired
	// "flush before close" ordering, not a post-close append.
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// Reset clears the closed state so the batcher can be reused for a new
// stream round (TUI calls this on OnStart / OnRoundStart).
func (b *TokenBatcher) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = false
}

func (b *TokenBatcher) appendLocked(think bool, text string) {
	if b.closed || text == "" {
		return
	}
	n := len(b.segs)
	if n > 0 && b.segs[n-1].think == think {
		b.segs[n-1].chunks = append(b.segs[n-1].chunks, text)
	} else {
		b.segs = append(b.segs, seg{think: think, chunks: []string{text}})
	}
	b.scheduleFlushLocked()
}

func (b *TokenBatcher) scheduleFlushLocked() {
	if b.timer != nil {
		return
	}
	b.timer = time.AfterFunc(b.interval, b.Flush)
}
