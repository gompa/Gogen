package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTokenBatcherPreservesThinkThenContentOrder(t *testing.T) {
	var msgs []tea.Msg
	b := &tokenBatcher{send: func(msg tea.Msg) { msgs = append(msgs, msg) }}

	// Typical batch window: reasoning arrives first, then content.
	b.thinkToken("Let me ")
	b.thinkToken("think")
	b.streamToken("Hello")
	b.flush()

	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2: %#v", len(msgs), msgs)
	}
	think, ok := msgs[0].(streamThinkingMsg)
	if !ok || think.token != "Let me think" {
		t.Fatalf("msg[0] = %#v, want thinking %q", msgs[0], "Let me think")
	}
	content, ok := msgs[1].(streamTokenMsg)
	if !ok || content.token != "Hello" {
		t.Fatalf("msg[1] = %#v, want token %q", msgs[1], "Hello")
	}
}

func TestTokenBatcherPreservesInterleavedSegments(t *testing.T) {
	var msgs []tea.Msg
	b := &tokenBatcher{send: func(msg tea.Msg) { msgs = append(msgs, msg) }}

	b.thinkToken("A")
	b.streamToken("B")
	b.thinkToken("C")
	b.flush()

	if len(msgs) != 3 {
		t.Fatalf("got %d msgs, want 3: %#v", len(msgs), msgs)
	}
	if got, ok := msgs[0].(streamThinkingMsg); !ok || got.token != "A" {
		t.Fatalf("msg[0] = %#v", msgs[0])
	}
	if got, ok := msgs[1].(streamTokenMsg); !ok || got.token != "B" {
		t.Fatalf("msg[1] = %#v", msgs[1])
	}
	if got, ok := msgs[2].(streamThinkingMsg); !ok || got.token != "C" {
		t.Fatalf("msg[2] = %#v", msgs[2])
	}
}
