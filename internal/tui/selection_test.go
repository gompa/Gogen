package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

func TestExtractLeadingSGR(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantSGR   string
		wantEmpty bool
	}{
		{
			name:    "gray foreground",
			input:   "\x1b[38;2;136;136;136m<thinking>test</thinking>\x1b[0m",
			wantSGR: "\x1b[38;2;136;136;136m",
		},
		{
			name:    "bold + color",
			input:   "\x1b[1m\x1b[38;2;0;170;170mGoGen:\x1b[0m",
			wantSGR: "\x1b[1m\x1b[38;2;0;170;170m",
		},
		{
			name:      "plain text",
			input:     "just plain text",
			wantEmpty: true,
		},
		{
			name:      "empty string",
			input:     "",
			wantEmpty: true,
		},
		{
			name:      "SGR not at start",
			input:     "hello \x1b[38mworld",
			wantEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLeadingSGR(tt.input)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("expected empty, got %q", got)
				}
				return
			}
			if got != tt.wantSGR {
				t.Fatalf("expected %q, got %q", tt.wantSGR, got)
			}
		})
	}
}

func TestSGRPropagation(t *testing.T) {
	sgr := extractLeadingSGR("\x1b[38;2;136;136;136m<thinking>test</thinking>\x1b[0m")
	if sgr == "" {
		t.Fatal("expected non-empty SGR")
	}

	t.Run("thinking block — SGR open at split", func(t *testing.T) {
		// Simulate wordwrap splitting a fully-styled thinking line.
		// Part 0 has no \x1b[0m → style is still open → propagate.
		parts := []string{
			"\x1b[38;2;136;136;136m<thinking>very long",
			"reasoning text</thinking>\x1b[0m",
		}
		if strings.Contains(parts[0], "\x1b[0m") {
			t.Fatal("part 0 should NOT have reset for this test to be meaningful")
		}
		for i := 1; i < len(parts); i++ {
			parts[i] = sgr + parts[i] + "\x1b[0m"
		}
		cont := parts[1]
		if !strings.HasPrefix(cont, sgr) {
			t.Fatalf("continuation missing SGR prefix: %q", cont)
		}
		if !strings.HasSuffix(cont, "\x1b[0m") {
			t.Fatalf("continuation missing reset suffix: %q", cont)
		}
	})

	t.Run("assistant message — SGR closed at split", func(t *testing.T) {
		// Simulate wordwrap splitting a partially-styled line where
		// the label SGR was already closed before the wrap point.
		parts := []string{
			"\x1b[1m\x1b[38;2;0;170;170mGoGen:\x1b[0m long",
			"message text continues here",
		}
		if !strings.Contains(parts[0], "\x1b[0m") {
			t.Fatal("part 0 SHOULD have reset for this test to be meaningful")
		}
		// Fix: skip propagation for this case
		shouldPropagate := !strings.Contains(parts[0], "\x1b[0m")
		if shouldPropagate {
			t.Fatal("should NOT propagate SGR when reset is present in part 0")
		}
		// Continuation line should remain unstyled
		cont := parts[1]
		if strings.HasPrefix(cont, "\x1b[") {
			t.Fatalf("continuation should NOT have ANSI prefix: %q", cont)
		}
	})

	t.Run("tool call — multi-styled, SGR closed early", func(t *testing.T) {
		// Tool call prefix SGR closes before the args start.
		// Part 0 has \x1b[0m → don't propagate prefix SGR.
		parts := []string{
			"\x1b[38;2;204;170;0m  →\x1b[0m name \x1b[38;2;136;136;136mvery",
			"long args here\x1b[0m",
		}
		if !strings.Contains(parts[0], "\x1b[0m") {
			t.Fatal("part 0 SHOULD have reset")
		}
		shouldPropagate := !strings.Contains(parts[0], "\x1b[0m")
		if shouldPropagate {
			t.Fatal("should NOT propagate when reset is in part 0")
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

func TestIsCtrlShiftC(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.Msg
		want bool
	}{
		{
			name: "kitty CSI",
			msg:  []byte("\x1b[99;6u"),
			want: true,
		},
		{
			name: "xterm modifyOtherKeys",
			msg:  []byte("\x1b[27;6;99~"),
			want: true,
		},
		{
			name: "plain ctrl+c key",
			msg:  tea.KeyMsg{Type: tea.KeyCtrlC},
			want: false,
		},
		{
			name: "unrelated CSI",
			msg:  []byte("\x1b[99;5u"),
			want: false,
		},
		{
			name: "ctrl+shift+left key",
			msg:  tea.KeyMsg{Type: tea.KeyCtrlShiftLeft},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCtrlShiftC(tt.msg); got != tt.want {
				t.Fatalf("isCtrlShiftC() = %v, want %v", got, tt.want)
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
	consumed := m.handleMouseSelection(tea.MouseMsg{
		Action: tea.MouseActionRelease,
		Button: tea.MouseButtonLeft,
		X:      5,
		Y:      0,
	})
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
	// Some terminals emit ButtonNone motion while the button is down / on release.
	consumed := m.handleMouseSelection(tea.MouseMsg{
		Action: tea.MouseActionMotion,
		Button: tea.MouseButtonNone,
		X:      6,
		Y:      0,
	})
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
	consumed := m.handleMouseSelection(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      leftPad + 3,
		Y:      0,
	})
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
