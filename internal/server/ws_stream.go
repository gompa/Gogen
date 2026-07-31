package server

import (
	"context"
	"sync"
	"time"
)

const wsTokenFlushInterval = 16 * time.Millisecond

// wsConnStream owns the in-flight stream cancel handles for one WebSocket
// connection. Allocated on the heap so the read loop and stream goroutine
// share clear ownership (not ad-hoc locals).
//
// Two cancel functions allow close() to stop only the LLM call while keeping
// the outer goroutine context alive so deferred cleanup (stream.end(),
// turnMu.Unlock(), turn_end write) runs naturally.
type wsConnStream struct {
	mu        sync.Mutex
	cancel    context.CancelFunc // outer: cancels the stream goroutine (cancelInFlight)
	llmCancel context.CancelFunc // inner: cancels only the LLM call (close)
	errCh     chan error
}

// cancelInFlight stops the entire stream: cancels the outer context (which
// propagates to the inner LLM context) and waits for the stream goroutine
// to finish its deferred cleanup.
//
// If the stream does not exit within wsStreamDrainWait, errCh is kept so a
// later cancel/message can keep waiting. Clearing it on timeout made the next
// tryAcquireTurn fail with "agent is busy" while a stuck command still held
// turnMu.
func (s *wsConnStream) cancelInFlight() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
		s.llmCancel = nil
	}
	prevErr := s.errCh
	s.mu.Unlock()
	if prevErr == nil {
		return
	}
	// Wait for cancel repair (appendCanceledToolResults + FlushSession).
	if drainStreamErr(prevErr) {
		s.mu.Lock()
		if s.errCh == prevErr {
			s.errCh = nil
		}
		s.mu.Unlock()
	}
}

// close cancels only the inner LLM context so StreamProcessInput returns
// promptly. The outer goroutine context remains valid, so its deferred
// cleanup (stream.end(), turnMu.Unlock(), turn_end notification) always
// executes. Unlike cancelInFlight, it does not wait for the goroutine —
// the deferred cleanup runs asynchronously.
func (s *wsConnStream) close() {
	s.mu.Lock()
	// Cancel the inner (LLM) context only — the outer goroutine still needs
	// its context alive to run deferred cleanup.
	if s.llmCancel != nil {
		s.llmCancel()
		s.llmCancel = nil
	}
	// NEVER clear s.cancel or s.errCh here — the outer goroutine still
	// references them for its deferred cleanup.
	s.mu.Unlock()
}

// begin registers cancel handles for a new stream. Caller must already have
// cancelled any prior stream. Returns the error channel the stream goroutine
// should signal on exit.
func (s *wsConnStream) begin(cancel, llmCancel context.CancelFunc) chan error {
	s.mu.Lock()
	s.cancel = cancel
	s.llmCancel = llmCancel
	s.errCh = make(chan error, 1)
	errCh := s.errCh
	s.mu.Unlock()
	return errCh
}

func (s *wsConnStream) end() {
	s.mu.Lock()
	s.cancel = nil
	s.llmCancel = nil
	s.errCh = nil
	s.mu.Unlock()
}
