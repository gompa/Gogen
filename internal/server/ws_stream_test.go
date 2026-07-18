package server

import "testing"

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
