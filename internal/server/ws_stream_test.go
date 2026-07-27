package server

import (
	"testing"
	"time"

	"gogen/internal/streamutil"
)

func TestWSTokenBatcherPreservesThinkThenContentOrder(t *testing.T) {
	var got []WSMessage
	b := streamutil.NewTokenBatcher(func(think bool, text string) {
		if think {
			got = append(got, WSMessage{Type: "thinking_token", Content: text})
		} else {
			got = append(got, WSMessage{Type: "stream", Content: text})
		}
	}, 10*time.Millisecond)

	b.ThinkToken("Let me ")
	b.ThinkToken("think")
	b.StreamToken("Hello")
	b.Flush()

	if len(got) != 2 {
		t.Fatalf("got %d msgs, want 2: %#v", len(got), got)
	}
	if got[0].Type != "thinking_token" || got[0].Content != "Let me think" {
		t.Fatalf("msg[0] = %#v", got[0])
	}
	if got[1].Type != "stream" || got[1].Content != "Hello" {
		t.Fatalf("msg[1] = %#v", got[1])
	}
}
