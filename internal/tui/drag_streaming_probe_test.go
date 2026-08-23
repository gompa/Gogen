package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Probe: does the drag handler work while streaming, and what does one
// drag-motion event cost on a long conversation?
func TestProbeDragWhileStreaming(t *testing.T) {
	m := dragModel(t)
	m.sidebarMainLines = 30

	// A long conversation: ~200 chat lines of tool-result-like content.
	for i := 0; i < 200; i++ {
		m.appendChatLine("tool result " + strings.Repeat("x", 900))
	}
	m.setViewportContent()

	// Active streaming state: focused session mid-turn, spinner animating.
	m.streaming = true
	m.progressPhase = progressThinking
	m.setProgress(progressThinking, "thinking")

	// 1) Handler logic: press → motion → release must resize.
	border := m.sidebarWidth - 1
	if !m.handleSidebarResizeMouse(evDrag(mousePress, border)) {
		t.Fatal("border press not consumed while streaming")
	}
	if !m.handleSidebarResizeMouse(evDrag(mouseMotion, 34)) {
		t.Fatal("motion not consumed while streaming")
	}
	if m.sidebarWidth != 35 {
		t.Fatalf("width = %d, want 35 (handler logic works while streaming)", m.sidebarWidth)
	}
	m.handleSidebarResizeMouse(mouseEvent{button: tea.MouseLeft, kind: mouseRelease})
	if m.sidebarDragging {
		t.Fatal("dragging flag stuck")
	}

	// 2) Cost of ONE drag-motion event (SetSize → full re-wrap).
	start := time.Now()
	m.handleSidebarResizeMouse(evDrag(mousePress, m.sidebarWidth-1))
	m.handleSidebarResizeMouse(evDrag(mouseMotion, 40))
	one := time.Since(start)
	m.handleSidebarResizeMouse(mouseEvent{button: tea.MouseLeft, kind: mouseRelease})

	t.Logf("one drag-motion event on ~180KB conversation: %v", one)
	// A fast drag emits one motion event per cell moved; 30 cells across
	// the panel is a normal drag distance.
	t.Logf("30-cell drag would take ≈ %v of event-loop time", one*30)
}
