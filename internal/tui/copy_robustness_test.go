package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The copy chord's failure modes that only show up in a real terminal: the
// shape the chord arrives in, a drag whose release never arrives, and a
// selection that a rebuild moves out from under the user. Each of these ends
// with the same report — "ctrl+shift+c does nothing" — so each gets a
// regression here.

// TestCopyChordMatchingShapes also covers the SHIFTED-CODE encodings: a
// terminal that reports the shifted key as the main code (kitty's alternate
// key used alone: CSI 67;6u, xterm modifyOtherKeys with the shifted code
// point: CSI 27;6;67~) decodes to {Code:'C', Mod: ctrl|shift}, which every
// string form spells "ctrl+shift+C". The case-insensitive keystroke compare is
// what keeps the chord alive there; a bare ctrl+c must still be cancel.
func TestCopyChordMatchingShapedEncodings(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{"kitty uppercase code", tea.KeyPressMsg{Code: 'C', Mod: tea.ModCtrl | tea.ModShift}, true},
		{"xterm modifyOtherKeys uppercase code + text", tea.KeyPressMsg{Code: 'C', Text: "C", Mod: tea.ModCtrl | tea.ModShift}, true},
		{"kitty shifted-code field", tea.KeyPressMsg{Code: 'c', ShiftedCode: 'C', Mod: tea.ModCtrl | tea.ModShift}, true},
		{"associated text is the control byte", tea.KeyPressMsg{Code: 'c', Text: "\x03", Mod: tea.ModCtrl | tea.ModShift}, true},
		{"alt+ctrl+shift+c is not the chord", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift | tea.ModAlt}, false},
		{"ctrl+c stays cancel", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, false},
		{"ctrl+shift+u is not the chord", tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl | tea.ModShift}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{keys: DefaultKeyMap}
			if got := m.matchesCopyChord(tc.msg); got != tc.want {
				t.Fatalf("matchesCopyChord(%q) = %v, want %v", tc.msg.Keystroke(), got, tc.want)
			}
		})
	}
}

// TestLostReleaseDoesNotWedgeSelection: a drag whose release never arrives (the
// pointer left the window, the terminal dropped it, or a modal was up) used to
// leave Dragging set forever — every later press was swallowed and the next
// drag kept the stale anchor, so the first selection after a lost release
// copied the wrong range or nothing at all. A press is a fresh anchor.
func TestLostReleaseDoesNotWedgeSelection(t *testing.T) {
	m := copyTestModel(t)

	// A drag with no release: press row 0, move, then nothing.
	m.handleMouseMsg(tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft})
	m.handleMouseMsg(tea.MouseMotionMsg{X: 6, Y: 0, Button: tea.MouseLeft})
	if m.selection == nil || !m.selection.Dragging {
		t.Fatal("setup: drag did not start")
	}

	// The next press must start a NEW drag at the pressed cell.
	m.handleMouseMsg(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	if m.selection == nil || !m.selection.Dragging {
		t.Fatal("press after a lost release did not start a fresh drag")
	}
	if m.selection.StartY != 1 || m.selection.StartX != 0 {
		t.Fatalf("fresh drag anchored at (%d,%d), want (0,1) — the stale anchor survived",
			m.selection.StartX, m.selection.StartY)
	}

	m.handleMouseMsg(tea.MouseMotionMsg{X: 7, Y: 1, Button: tea.MouseLeft})
	m.handleMouseMsg(tea.MouseReleaseMsg{X: 7, Y: 1, Button: tea.MouseLeft})
	if got, want := m.getSelectedText(), "second"; got != want {
		t.Fatalf("selected text = %q, want %q", got, want)
	}
	// Scrolling is unlocked again (a stuck drag blocked the wheel).
	if m.handleMouseSelection(mouseEvent{x: 1, y: 1, kind: mouseWheelEvent}) {
		t.Fatal("wheel still blocked after the drag was recovered")
	}
}

// TestCopySurvivesRewrittenRow: a streaming write that rewrites the selected
// row (a growing tool diff, a re-rendered result) used to drop the highlight
// and leave the copy chord on "No selection" while the picked text was still
// on screen. The selection now re-anchors onto that text.
func TestCopySurvivesRewrittenRow(t *testing.T) {
	writes := stubClipboard(t, nil)
	m := copyTestModel(t)

	dragSelect(m) // selects "hello" of row 0
	m.chatLines[0] = "hello world, rewritten mid-stream"
	m.setViewportContent()

	if m.selection == nil {
		t.Fatal("selection dropped by a rewritten row that still holds the picked text")
	}
	if got, want := m.getSelectedText(), "hello"; got != want {
		t.Fatalf("re-anchored selection = %q, want %q", got, want)
	}
	pressCopy(m)
	if len(*writes) != 1 || (*writes)[0] != "hello" {
		t.Fatalf("clipboard writes = %v, want exactly [hello]", *writes)
	}
}

// TestCopyAfterDroppedSelectionExplains: when the picked text really is gone,
// the drop is reported on the status line — a silent drop is indistinguishable
// from a dead keybinding, which is how this was reported.
func TestCopyAfterDroppedSelectionExplains(t *testing.T) {
	m := copyTestModel(t)

	dragSelect(m)
	m.chatLines[0] = "completely different text"
	m.setViewportContent()

	if m.selection != nil {
		t.Fatalf("selection kept although its text is gone (%q)", m.getSelectedText())
	}
	if !strings.Contains(m.statusMsg, "Selection cleared") {
		t.Fatalf("status = %q, want a note that the selection was dropped", m.statusMsg)
	}
	// The chord still explains itself afterwards.
	pressCopy(m)
	if !strings.Contains(m.statusMsg, "No selection") {
		t.Fatalf("status after copy press = %q, want the empty-selection hint", m.statusMsg)
	}
}

// TestModalViewKeepsTerminalModes: the modal overlay is a different View, but
// it must keep the terminal's mouse/focus reporting. Dropping MouseMode while a
// modal was up stopped the terminal from sending mouse events — including the
// release of a drag in flight — which left the selection stuck mid-drag.
func TestModalViewKeepsTerminalModes(t *testing.T) {
	m := newSidebarTestModel(80)
	m.sidebarVisible = false
	m.SetSize(80, 24)

	plain := m.View()
	if plain.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("normal view MouseMode = %v, want cell motion", plain.MouseMode)
	}
	if !plain.ReportFocus {
		t.Fatal("normal view must request focus reporting")
	}

	m.modal = ModalHelp
	overlay := m.View()
	if overlay.MouseMode != plain.MouseMode {
		t.Fatalf("modal view MouseMode = %v, want %v (terminal mouse reporting must stay on)",
			overlay.MouseMode, plain.MouseMode)
	}
	if overlay.ReportFocus != plain.ReportFocus {
		t.Fatalf("modal view ReportFocus = %v, want %v", overlay.ReportFocus, plain.ReportFocus)
	}
}

// TestCopySurvivesBlankRowInSelection: the anchor text used to be built by
// getSelectedText, which SKIPS a blank row, while reanchorSelection searches
// the new rows joined one line per row. A selection spanning a blank row was
// therefore anchored on "…\n…" and searched inside "…\n\n…" — it never
// matched, so a row inserted above the selection dropped the highlight with
// "Selection cleared" although every picked character was still on screen.
// The clipboard output must NOT change: blank rows stay out of the pasted
// text, only the anchor accounts for them.
func TestCopySurvivesBlankRowInSelection(t *testing.T) {
	writes := stubClipboard(t, nil)
	m := newSidebarTestModel(80)
	m.sidebarVisible = false
	m.keys = DefaultKeyMap
	m.SetSize(80, 24)
	// Row 2 is blank: the selection below spans it.
	m.chatLines = []string{"header", "alpha", "", "beta"}
	m.setViewportContent()

	// Press at the start of "alpha" (row 1) and drag past the end of "beta"
	// (row 3). ViewportStyle pads one cell on the left, so terminal column 1
	// is content column 0.
	m.handleMouseMsg(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.handleMouseMsg(tea.MouseMotionMsg{X: 5, Y: 3, Button: tea.MouseLeft})
	m.handleMouseMsg(tea.MouseReleaseMsg{X: 5, Y: 3, Button: tea.MouseLeft})
	if m.selection == nil {
		t.Fatal("setup: the drag did not select anything")
	}
	if got, want := m.getSelectedText(), "alpha\nbeta"; got != want {
		t.Fatalf("selected text = %q, want %q (a blank row must not be copied)", got, want)
	}

	// A line inserted ABOVE the selection moves every row it covers, so the
	// row-by-row comparison fails and the selection must re-anchor by text.
	m.chatLines = append([]string{"newest line"}, m.chatLines...)
	m.setViewportContent()

	if m.selection == nil {
		t.Fatalf("selection dropped although its text is still on screen (status: %q)", m.statusMsg)
	}
	if got, want := m.getSelectedText(), "alpha\nbeta"; got != want {
		t.Fatalf("re-anchored selection = %q, want %q", got, want)
	}
	pressCopy(m)
	if len(*writes) != 1 || (*writes)[0] != "alpha\nbeta" {
		t.Fatalf("clipboard writes = %v, want exactly [alpha\\nbeta]", *writes)
	}
}
