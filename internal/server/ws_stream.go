package server

import (
	"strings"
	"sync"
	"time"
)

const wsTokenFlushInterval = 16 * time.Millisecond

// wsTokenBatcher coalesces stream/thinking tokens so the LLM reader is not
// blocked waiting on slow websocket clients.
type wsTokenBatcher struct {
	send   func(WSMessage)
	mu     sync.Mutex
	stream strings.Builder
	think  strings.Builder
	timer  *time.Timer
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
	if b.stream.Len() > 0 {
		b.send(WSMessage{Type: "stream", Content: b.stream.String()})
		b.stream.Reset()
	}
	if b.think.Len() > 0 {
		b.send(WSMessage{Type: "thinking_token", Content: b.think.String()})
		b.think.Reset()
	}
}

func (b *wsTokenBatcher) streamToken(token string) {
	if token == "" {
		return
	}
	b.mu.Lock()
	b.stream.WriteString(token)
	b.scheduleFlushLocked()
	b.mu.Unlock()
}

func (b *wsTokenBatcher) thinkToken(token string) {
	if token == "" {
		return
	}
	b.mu.Lock()
	b.think.WriteString(token)
	b.scheduleFlushLocked()
	b.mu.Unlock()
}
