package server

import (
	"context"
	"sync"
	"time"
)

const wsTokenFlushInterval = 16 * time.Millisecond

// wsConnStream owns the in-flight stream cancel handle for one WebSocket
// connection. Allocated on the heap so the read loop and stream goroutine
// share clear ownership (not ad-hoc locals).
type wsConnStream struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	errCh  chan error
}

func (s *wsConnStream) cancelInFlight() {
	s.mu.Lock()
	if s.cancel == nil {
		s.mu.Unlock()
		return
	}
	s.cancel()
	prevErr := s.errCh
	s.cancel = nil
	s.errCh = nil
	s.mu.Unlock()
	drainStreamErr(prevErr)
}

func (s *wsConnStream) close() {
	s.mu.Lock()
	if s.cancel == nil {
		s.mu.Unlock()
		return
	}
	s.cancel()
	prevErr := s.errCh
	s.cancel = nil
	s.errCh = nil
	s.mu.Unlock()
	drainStreamErr(prevErr)
}

// begin registers a new stream cancel handle. Caller must already have
// cancelled any prior stream. Returns the error channel the stream
// goroutine should signal on exit.
func (s *wsConnStream) begin(cancel context.CancelFunc) chan error {
	s.mu.Lock()
	s.cancel = cancel
	s.errCh = make(chan error, 1)
	errCh := s.errCh
	s.mu.Unlock()
	return errCh
}

func (s *wsConnStream) end() {
	s.mu.Lock()
	s.cancel = nil
	s.errCh = nil
	s.mu.Unlock()
}

type wsTokenSeg struct {
	think bool
	text  string
}

// wsTokenBatcher coalesces stream/thinking tokens so the LLM reader is not
// blocked waiting on slow websocket clients.
// Segments flush in arrival order so thinking is not emitted after content
// when both were buffered in the same window.
type wsTokenBatcher struct {
	send  func(WSMessage)
	mu    sync.Mutex
	segs  []wsTokenSeg
	timer *time.Timer
}

func newWSTokenBatcher(send func(WSMessage)) *wsTokenBatcher {
	return &wsTokenBatcher{send: send}
}

func (b *wsTokenBatcher) scheduleFlushLocked() {
	if b.timer != nil {
		return
	}
	b.timer = time.AfterFunc(wsTokenFlushInterval, b.flush)
}

func (b *wsTokenBatcher) flush() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	segs := b.segs
	b.segs = nil
	b.mu.Unlock()

	for _, seg := range segs {
		if seg.text == "" {
			continue
		}
		if seg.think {
			b.send(WSMessage{Type: "thinking_token", Content: seg.text})
		} else {
			b.send(WSMessage{Type: "stream", Content: seg.text})
		}
	}
}

func (b *wsTokenBatcher) appendLocked(think bool, token string) {
	n := len(b.segs)
	if n > 0 && b.segs[n-1].think == think {
		b.segs[n-1].text += token
	} else {
		b.segs = append(b.segs, wsTokenSeg{think: think, text: token})
	}
	b.scheduleFlushLocked()
}

func (b *wsTokenBatcher) streamToken(token string) {
	if token == "" {
		return
	}
	b.mu.Lock()
	b.appendLocked(false, token)
	b.mu.Unlock()
}

func (b *wsTokenBatcher) thinkToken(token string) {
	if token == "" {
		return
	}
	b.mu.Lock()
	b.appendLocked(true, token)
	b.mu.Unlock()
}
