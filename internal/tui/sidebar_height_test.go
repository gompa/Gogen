package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"gogen/internal/agent"
)

// The sidebar must match the MAIN COLUMN's exact row count: one extra row
// grows the combined frame past the terminal, and with alt-screen off that
// scrolls the whole view upward (reported as "chat shifted up / empty
// panel").
func TestSidebarHeightMatchesMainColumn(t *testing.T) {
	m := newSidebarTestModel(100)
	m.height = 24
	m.SetSize(100, 24)
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.lives.Add(&agent.Agent{}, "bg")
	m.lives.ByID("s2").streaming = true

	main := m.renderMainColumn()
	mainLines := strings.Count(main, "\n") + 1

	panel := m.renderSidebar(mainLines)

	t.Run("panel row count equals main column", func(t *testing.T) {
		if got := strings.Count(panel, "\n") + 1; got != mainLines {
			t.Fatalf("panel lines = %d, main lines = %d", got, mainLines)
		}
	})

	t.Run("no trailing newline", func(t *testing.T) {
		if strings.HasSuffix(panel, "\n") {
			t.Fatal("trailing newline adds a phantom blank row to the frame")
		}
	})

	t.Run("combined frame does not grow", func(t *testing.T) {
		combined := lipgloss.JoinHorizontal(lipgloss.Top, panel, main)
		if got := strings.Count(combined, "\n") + 1; got != mainLines {
			t.Fatalf("combined frame = %d rows, want %d", got, mainLines)
		}
	})

	t.Run("header survives at exact height", func(t *testing.T) {
		lines := strings.Split(panel, "\n")
		if !strings.Contains(stripANSI(lines[0]), "┌") {
			t.Fatalf("first panel row must be the top border: %q", stripANSI(lines[0]))
		}
		if !strings.Contains(stripANSI(lines[1]), "SESSIONS") {
			t.Fatalf("second panel row = %q", stripANSI(lines[1]))
		}
		if !strings.Contains(stripANSI(lines[len(lines)-1]), "┘") {
			t.Fatalf("last panel row must be the bottom border: %q", stripANSI(lines[len(lines)-1]))
		}
	})
}

// Overflow clip: more sessions than rows must never grow the panel.
func TestSidebarClipsOverflow(t *testing.T) {
	m := newSidebarTestModel(100)
	m.height = 10
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	for i := 0; i < 50; i++ {
		m.lives.Add(&agent.Agent{}, "x")
	}

	panel := m.renderSidebar(8)
	if got := strings.Count(panel, "\n") + 1; got != 8 {
		t.Fatalf("panel lines = %d, want 8", got)
	}
}
