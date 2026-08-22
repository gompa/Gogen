package tui

import (
	"testing"
	"time"

	"gogen/internal/streamutil"

	tea "charm.land/bubbletea/v2"
)

func TestTokenBatcherPreservesThinkThenContentOrder(t *testing.T) {
	var all []tea.Msg
	b := streamutil.NewTokenBatcher(func(think bool, text string) {
		if think {
			all = append(all, streamThinkingMsg{token: text})
		} else {
			all = append(all, streamTokenMsg{token: text})
		}
	}, 10*time.Millisecond)

	// Typical batch window: reasoning arrives first, then content.
	b.ThinkToken("Let me ")
	b.ThinkToken("think")
	b.StreamToken("Hello")
	b.Flush()

	if len(all) != 2 {
		t.Fatalf("got %d msgs, want 2: %#v", len(all), all)
	}
	m, ok := all[0].(streamThinkingMsg)
	if !ok || m.token != "Let me think" {
		t.Fatalf("msg[0] = %#v, want thinking %q", all[0], "Let me think")
	}
	m2, ok := all[1].(streamTokenMsg)
	if !ok || m2.token != "Hello" {
		t.Fatalf("msg[1] = %#v, want token %q", all[1], "Hello")
	}
}

func TestTokenBatcherPreservesInterleavedSegments(t *testing.T) {
	var all []tea.Msg
	b := streamutil.NewTokenBatcher(func(think bool, text string) {
		if think {
			all = append(all, streamThinkingMsg{token: text})
		} else {
			all = append(all, streamTokenMsg{token: text})
		}
	}, 10*time.Millisecond)

	b.ThinkToken("A")
	b.StreamToken("B")
	b.ThinkToken("C")
	b.Flush()

	if len(all) != 3 {
		t.Fatalf("got %d msgs, want 3: %#v", len(all), all)
	}
	if m, ok := all[0].(streamThinkingMsg); !ok || m.token != "A" {
		t.Fatalf("msg[0] = %#v", all[0])
	}
	if m, ok := all[1].(streamTokenMsg); !ok || m.token != "B" {
		t.Fatalf("msg[1] = %#v", all[1])
	}
	if m, ok := all[2].(streamThinkingMsg); !ok || m.token != "C" {
		t.Fatalf("msg[2] = %#v", all[2])
	}
}
