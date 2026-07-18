package tui

import (
	"strings"
	"testing"
)

func TestThinkingClosesBeforeAssistantContent(t *testing.T) {
	m := Model{
		chatLines:           nil,
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
	}

	m.handleStreamThinking("step one")
	m.handleStreamToken("Hello")

	if len(m.chatLines) != 2 {
		t.Fatalf("chatLines=%d want 2: %#v", len(m.chatLines), m.chatLines)
	}
	if !strings.Contains(m.chatLines[0], "<thinking>step one</thinking>") {
		t.Fatalf("thinking line = %q", m.chatLines[0])
	}
	if !strings.Contains(m.chatLines[1], "Hello") {
		t.Fatalf("assistant line = %q", m.chatLines[1])
	}
	if m.streamThinkingOpen {
		t.Fatal("thinking should be closed")
	}
}

func TestAssistantContinuesAfterLateThinking(t *testing.T) {
	m := Model{
		chatLines:           nil,
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
	}

	// Content first, then a late thinking segment, then more content.
	// After thinking closes, subsequent content creates a new line so that
	// temporal order is preserved: content → thinking → content.
	m.handleStreamToken("Hello ")
	m.handleStreamThinking("aside")
	m.handleStreamToken("world")

	if len(m.chatLines) != 3 {
		t.Fatalf("chatLines=%d want 3: %#v", len(m.chatLines), m.chatLines)
	}
	if !strings.Contains(m.chatLines[0], "Hello ") {
		t.Fatalf("line 0 = %q, want content before thinking", m.chatLines[0])
	}
	if !strings.Contains(m.chatLines[1], "<thinking>aside</thinking>") {
		t.Fatalf("line 1 = %q, want thinking block", m.chatLines[1])
	}
	if !strings.Contains(m.chatLines[2], "world") {
		t.Fatalf("line 2 = %q, want content after thinking", m.chatLines[2])
	}
}
