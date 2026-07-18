package server

import (
	"sync"
	"time"
)

const wsTokenFlushInterval = 16 * time.Millisecond

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
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	for _, seg := range b.segs {
		if seg.text == "" {
			continue
		}
		if seg.think {
			b.send(WSMessage{Type: "thinking_token", Content: seg.text})
		} else {
			b.send(WSMessage{Type: "stream", Content: seg.text})
		}
	}
	b.segs = b.segs[:0]
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
