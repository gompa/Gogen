package server

import (
	"sync/atomic"
	"testing"
)

func TestWSTokenBatcherFlushReleasesLockBeforeSend(t *testing.T) {
	var b *wsTokenBatcher
	var sendSawUnlocked atomic.Bool
	b = newWSTokenBatcher(func(WSMessage) {
		// TryLock succeeds only if flush released b.mu before send.
		if b.mu.TryLock() {
			sendSawUnlocked.Store(true)
			b.mu.Unlock()
		}
	})
	b.streamToken("hi")
	b.flush()
	if !sendSawUnlocked.Load() {
		t.Fatal("flush held b.mu across send")
	}
}

func TestWSTokenBatcherPreservesThinkThenContentOrder(t *testing.T) {
	var msgs []WSMessage
	b := newWSTokenBatcher(func(msg WSMessage) { msgs = append(msgs, msg) })

	b.thinkToken("Let me ")
	b.thinkToken("think")
	b.streamToken("Hello")
	b.flush()

	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2: %#v", len(msgs), msgs)
	}
	if msgs[0].Type != "thinking_token" || msgs[0].Content != "Let me think" {
		t.Fatalf("msg[0] = %#v", msgs[0])
	}
	if msgs[1].Type != "stream" || msgs[1].Content != "Hello" {
		t.Fatalf("msg[1] = %#v", msgs[1])
	}
}
