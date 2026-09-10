package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Regression: a tab in a chat line is measured as one cell by wrapLine
// (ansi.Wrap) and as zero cells by ansi.StringWidth, but the viewport's
// View render expands it to lipgloss's default TabWidth of four spaces
// BEFORE its own wrap pass. A line with a tab near the wrap boundary
// therefore passed wrapLine as a single ≤limit part and then re-wrapped
// inside View onto two terminal rows: every row below it shifted down one,
// scrolling landed on the wrong row, and the top of the message appeared
// stuck. These tests pin that every wrapped part renders through the
// viewport style onto exactly one row.

func wrapTabTestModel() *Model {
	m := newSidebarTestModel(100)
	m.viewport.Width = 42 // wrapWidth = 42 - 2 padding = 40
	m.viewport.Style = ViewportStyle
	return m
}

func TestWrapLineTabsRenderOneRowPerPart(t *testing.T) {
	m := wrapTabTestModel()
	w := m.wrapWidth()

	cases := []struct {
		name string
		line string
	}{
		{"tab just before wrap boundary", strings.Repeat("x", 38) + "\tT"},
		{"leading tab pushes long word over", "\t" + strings.Repeat("y", 39)},
		{"tab mid line at boundary", strings.Repeat("a", 20) + "\t" + strings.Repeat("b", 19)},
		{"multiple tabs", "\t" + strings.Repeat("c", 17) + "\t" + strings.Repeat("d", 17)},
		{"trailing tab", strings.Repeat("e", 39) + "\t"},
		{"no tabs control", strings.Repeat("z", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := m.wrapLine(tc.line)
			for i, p := range parts {
				if got := ansi.StringWidth(p); got > w {
					t.Fatalf("part %d wider than wrapWidth: %d > %d (%q)", i, got, w, p)
				}
			}
			// Render exactly the way Viewport.View does: a lipgloss style
			// with the content width set (its default TabWidth expands
			// tabs, then it re-wraps anything wider than the width).
			out := lipgloss.NewStyle().
				Width(w).
				MaxWidth(w).
				Render(strings.Join(parts, "\n"))
			rows := strings.Split(out, "\n")
			if len(rows) != len(parts) {
				t.Fatalf("each wrapped part must render onto exactly one row: %d parts -> %d rows\nparts: %q\nrows:  %q",
					len(parts), len(rows), parts, rows)
			}
		})
	}
}

// TestViewportViewMatchesPerLineRender pins the same invariant end-to-end
// through the real Viewport.View: the rendered frame must be the per-line
// render of the stored lines (one row each, top-aligned), with no hidden
// re-wrap shifting rows below the offending line.
func TestViewportViewMatchesPerLineRender(t *testing.T) {
	m := wrapTabTestModel()
	w := m.wrapWidth()

	lines := []string{
		strings.Repeat("x", 38) + "\tT",
		"row below the tab line",
		"\t" + strings.Repeat("y", 39),
		"another row below",
	}
	// Publish the wrapped lines exactly like setViewportContent/buildFromPrefix.
	var stored []string
	maxW := 0
	for _, l := range lines {
		parts := m.wrapLine(l)
		stored = append(stored, parts...)
		for _, p := range parts {
			if got := ansi.StringWidth(p); got > maxW {
				maxW = got
			}
		}
	}
	m.viewport.SetContentLines(stored, maxW)
	m.viewport.Height = len(stored)

	// Per-line reference: inner wrap at the content width, then the
	// viewport's own padding style — one row per stored line.
	var perLine []string
	for _, l := range stored {
		perLine = append(perLine,
			ViewportStyle.Render(lipgloss.NewStyle().Width(w).MaxWidth(w).Render(l)))
	}
	want := strings.Join(perLine, "\n")
	if got := m.viewport.View(); got != want {
		t.Fatalf("viewport render diverged from per-line render (hidden re-wrap):\n got: %q\nwant: %q", got, want)
	}
}
