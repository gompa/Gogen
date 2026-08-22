package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func TestWrapLinePropagatesOpenStyles(t *testing.T) {
	const cyanBold = "\x1b[1;38;2;0;170;170m"
	const dim = "\x1b[38;2;136;136;136m"

	t.Run("thinking block — style open at split propagates", func(t *testing.T) {
		m := &Model{}
		m.viewport.Width = 42
		m.viewport.Style = ViewportStyle
		line := dim + "<thinking>" + strings.Repeat("reasoning text ", 8)
		parts := m.wrapLine(line)
		if len(parts) < 2 {
			t.Fatalf("expected wrapped parts, got %d", len(parts))
		}
		for i := 1; i < len(parts); i++ {
			if !strings.HasPrefix(parts[i], dim) {
				t.Fatalf("part %d missing dim prefix: %q", i, parts[i])
			}
			// Intermediate continuations are closed so padding cannot
			// inherit the style — accept either reset dialect. The LAST
			// part stays open when the source line does (same state as the
			// unwrapped input).
			if i < len(parts)-1 &&
				!strings.HasSuffix(parts[i], "\x1b[m") && !strings.HasSuffix(parts[i], "\x1b[0m") {
				t.Fatalf("part %d missing reset suffix: %q", i, parts[i])
			}
		}
	})

	t.Run("assistant label — closed style must NOT bleed onto continuations", func(t *testing.T) {
		// Regression for the lipgloss-v2 migration: v2 closes the label
		// with a bare "\x1b[m"; the old literal-reset heuristic treated the
		// style as open and painted every continuation line cyan/bold.
		m := &Model{}
		m.viewport.Width = 42
		m.viewport.Style = ViewportStyle
		line := cyanBold + "GoGen:\x1b[m " + strings.Repeat("plain reply text ", 10)
		parts := m.wrapLine(line)
		if len(parts) < 2 {
			t.Fatalf("expected wrapped parts, got %d", len(parts))
		}
		for i := 1; i < len(parts); i++ {
			if strings.Contains(parts[i], "38;2;0;170;170") {
				t.Fatalf("part %d leaked label style into body text: %q", i, parts[i])
			}
		}
	})

	t.Run("tool call — args style active at split propagates", func(t *testing.T) {
		m := &Model{}
		m.viewport.Width = 42
		m.viewport.Style = ViewportStyle
		line := "\x1b[38;2;204;170;0m  →\x1b[0m read_file " +
			dim + strings.Repeat("very long argument text ", 6) + "\x1b[0m"
		parts := m.wrapLine(line)
		if len(parts) < 3 {
			t.Fatalf("expected 3+ parts, got %d", len(parts))
		}
		for i := 2; i < len(parts); i++ {
			if !strings.HasPrefix(parts[i], dim) {
				t.Fatalf("part %d lost dim args styling: %q", i, parts[i])
			}
		}
	})

	t.Run("styled segment starts partway through a later line and itself spans 3+ wraps", func(t *testing.T) {
		// Regression test: a plain prefix + name is followed by a long
		// dimmed args value that only becomes active partway through the
		// second wrapped line, and itself needs several more wrapped
		// lines. Every wrapped line from the point the style first turns
		// up onward must carry it — previously the propagation logic only
		// looked at parts[0], so once the style appeared later than that,
		// none of the following continuation lines received it and the
		// styling silently vanished as soon as earlier lines scrolled out
		// of view.
		m := &Model{}
		m.viewport.Width = 42 // wrapWidth = 42 - 2 padding = 40
		m.viewport.Style = ViewportStyle

		dimSGR := "\x1b[38;2;136;136;136m"
		prefix := "\x1b[38;2;204;170;0m  →\x1b[0m read_file "
		args := dimSGR + strings.Repeat("very long argument text ", 6) + "\x1b[0m"
		line := prefix + args

		parts := m.wrapLine(line)
		if len(parts) < 4 {
			t.Fatalf("expected line to wrap into at least 4 parts, got %d: %#v", len(parts), parts)
		}

		seenStyle := false
		for i, p := range parts {
			if strings.Contains(p, dimSGR) {
				seenStyle = true
			}
			if seenStyle && p != "" && !strings.HasPrefix(p, "\x1b[") {
				t.Fatalf("part %d lost styling after the dim style should have carried forward: %q", i, p)
			}
		}
		if !seenStyle {
			t.Fatal("test setup issue: dim style never appeared in any wrapped part")
		}
	})
}

func TestWrapLineFitsWidth(t *testing.T) {
	m := &Model{}
	m.viewport.Width = 42 // wrapWidth = 42 - 2 padding = 40
	m.viewport.Style = ViewportStyle

	line := "See https://example.com/very/long/path/that/exceeds/forty/columns/easily for details"
	parts := m.wrapLine(line)
	w := m.wrapWidth()
	for i, p := range parts {
		plain := stripANSI(p)
		if got := len([]rune(plain)); got > w {
			t.Fatalf("part %d rune width %d > wrapWidth %d: %q", i, got, w, plain)
		}
	}
	if len(parts) < 3 {
		t.Fatalf("expected hard-wrap to split overlong URL, got %d parts: %#v", len(parts), parts)
	}
}

// TestCopySelectionBindingMatchesCtrlShiftC is the bubbletea-v2 migration
// regression test. On v1, ctrl+shift+c was byte-identical to ctrl+c in
// legacy terminal encoding (0x03), so pressing it quit the program instead
// of copying. v2 negotiates key disambiguation, delivering a distinct
// KeyPressMsg that the CopySelection binding must match — while plain
// ctrl+c keeps its cancel/quit role.
func TestCopySelectionBindingMatchesCtrlShiftC(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{
			name: "ctrl+shift+c matches copy binding",
			msg:  tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift},
			want: true,
		},
		{
			name: "plain ctrl+c does not match copy binding",
			msg:  tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
			want: false,
		},
		{
			name: "unmodified c does not match copy binding",
			msg:  tea.KeyPressMsg{Code: 'c'},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := key.Matches(tt.msg, DefaultKeyMap.CopySelection); got != tt.want {
				t.Fatalf("key.Matches(%q) = %v, want %v", tt.msg.String(), got, tt.want)
			}
		})
	}
}

func TestSliceByCellsUTF8(t *testing.T) {
	// "café" — é is multi-byte; cells 0..4 cover all 4 runes
	got := sliceByCells("café", 0, 4)
	if got != "café" {
		t.Fatalf("sliceByCells full = %q, want café", got)
	}
	got = sliceByCells("café", 3, 4)
	if got != "é" {
		t.Fatalf("sliceByCells last = %q, want é", got)
	}
	got = sliceByCells("café", 0, 3)
	if got != "caf" {
		t.Fatalf("sliceByCells prefix = %q, want caf", got)
	}
}

func TestGetSelectedTextUTF8(t *testing.T) {
	m := &Model{}
	m.wrappedContent = "café au lait"
	m.wrappedLinesDirty = true
	m.selection = &SelectionState{
		Active: true,
		StartX: 0,
		StartY: 0,
		EndX:   4, // cells covering "café"
		EndY:   0,
	}
	got := m.getSelectedText()
	if got != "café" {
		t.Fatalf("getSelectedText = %q, want café", got)
	}
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("rune count = %d, want 4", n)
	}
}

func TestCopySelectionKeepsUntilCopy(t *testing.T) {
	m := &Model{}
	m.wrappedContent = "hello world\nsecond line"
	m.wrappedLinesDirty = true
	m.selection = &SelectionState{
		Active:   true,
		Dragging: false,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	if got := m.getSelectedText(); got != "hello" {
		t.Fatalf("getSelectedText = %q, want %q", got, "hello")
	}
	if !m.hasSelection() {
		t.Fatal("expected hasSelection")
	}
}

func TestMouseReleaseDoesNotClearSelection(t *testing.T) {
	m := &Model{}
	m.viewport.Height = 20
	m.viewport.Width = 40
	m.viewport.Style = ViewportStyle
	m.wrappedContent = "hello world"
	m.wrappedLinesDirty = true
	m.selectionYOff = 0
	m.selection = &SelectionState{
		Active:   true,
		Dragging: true,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	ev, ok := normalizeMouseEvent(tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	if !ok {
		t.Fatal("expected release msg to normalize")
	}
	consumed := m.handleMouseSelection(ev)
	if !consumed {
		t.Fatal("expected release to be consumed")
	}
	if m.selection == nil || !m.selection.Active {
		t.Fatal("selection should remain after release")
	}
	if m.selection.Dragging {
		t.Fatal("dragging should be false after release")
	}
	if m.getSelectedText() != "hello" {
		t.Fatalf("selected text = %q", m.getSelectedText())
	}
}

func TestButtonNoneMotionDoesNotClearDrag(t *testing.T) {
	m := &Model{}
	m.viewport.Height = 20
	m.viewport.Width = 40
	m.viewport.Style = ViewportStyle
	m.wrappedContent = "hello world"
	m.wrappedLinesDirty = true
	m.selectionYOff = 0
	m.selection = &SelectionState{
		Active:   true,
		Dragging: true,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	// Some terminals emit button-less motion while the button is down / on release.
	ev, ok := normalizeMouseEvent(tea.MouseMotionMsg{X: 6, Y: 0, Button: tea.MouseNone})
	if !ok {
		t.Fatal("expected motion msg to normalize")
	}
	consumed := m.handleMouseSelection(ev)
	if !consumed {
		t.Fatal("expected motion to be consumed during drag")
	}
	if m.selection == nil || !m.selection.Active || !m.selection.Dragging {
		t.Fatal("ButtonNone motion must not clear an in-progress selection")
	}
	if m.getSelectedText() != "hello" {
		t.Fatalf("selected text = %q, want hello", m.getSelectedText())
	}
}

func TestMousePressRestartsSelection(t *testing.T) {
	m := &Model{}
	m.viewport.Height = 20
	m.viewport.Width = 40
	m.viewport.Style = ViewportStyle
	m.wrappedContent = "hello world"
	m.wrappedLinesDirty = true
	m.selection = &SelectionState{
		Active:   true,
		Dragging: false,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	// Left pad is 1 from ViewportStyle — click column 1+3 = content x 3
	leftPad := m.viewport.Style.GetPaddingLeft()
	ev, ok := normalizeMouseEvent(tea.MouseClickMsg{X: leftPad + 3, Y: 0, Button: tea.MouseLeft})
	if !ok {
		t.Fatal("expected click msg to normalize")
	}
	consumed := m.handleMouseSelection(ev)
	if !consumed {
		t.Fatal("expected press to be consumed")
	}
	if m.selection == nil || !m.selection.Dragging {
		t.Fatal("expected a new drag selection")
	}
	if m.selection.StartX != 3 || m.selection.StartY != 0 {
		t.Fatalf("start = (%d,%d), want (3,0)", m.selection.StartX, m.selection.StartY)
	}
}
