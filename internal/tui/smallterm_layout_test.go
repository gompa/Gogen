package tui

// Regression tests for the small-display layout guards. The TUI renders
// inline (alt-screen off), so ANY frame taller or wider than the terminal
// scrolls the screen and desyncs the incremental cell diff. These tests pin
// the three guards introduced after the layout review: the status bar never
// wrapping, bordered modals clamped to the terminal in both dimensions, and
// SetSize degrading the input band so the main column always fits.

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// frameDims reports the ANSI-aware visible cell dimensions of a rendered block.
func frameDims(s string) (int, int) {
	w, h := 0, 0
	for _, l := range strings.Split(s, "\n") {
		h++
		if lw := ansi.StringWidth(l); lw > w {
			w = lw
		}
	}
	return w, h
}

func TestStatusBarStatusMessageStaysOneLine(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	longMsg := "✗ Compact failed: context window exhausted before summarization could complete (request aborted mid-flight)"

	for _, tc := range []struct {
		name string
		w    int
	}{{"80 cols", 80}, {"50 cols", 50}, {"24 cols", 24}} {
		t.Run(tc.name, func(t *testing.T) {
			m.SetSize(tc.w, 24)
			m.statusMsg = longMsg
			bar := m.renderStatusBar()
			if lines := strings.Count(bar, "\n") + 1; lines != 1 {
				t.Fatalf("status bar wrapped to %d lines, want 1 (frame growth desyncs the renderer)", lines)
			}
			if w := lipgloss.Width(bar); w > tc.w {
				t.Fatalf("status bar is %d cells, want <= %d", w, tc.w)
			}
		})
	}
}

func TestBorderedModalWidthClamp(t *testing.T) {
	t.Run("clamped to terminal width", func(t *testing.T) {
		m := dragModel(t)
		m.width = 80
		m.height = 0 // isolate: height clamp off
		box := m.renderBorderedModal([]styleLine{
			{text: "Title", highlight: true},
			{text: strings.Repeat("x", 200)},
		})
		if w, _ := frameDims(box); w > 80 {
			t.Fatalf("modal box is %d cells, want <= 80", w)
		}
		if plain := stripANSI(box); !strings.Contains(plain, "Title") {
			t.Fatal("clamping lost the title row")
		}
	})

	t.Run("zero-size model keeps legacy unclamped layout", func(t *testing.T) {
		zero := &Model{} // width/height unset — tests build models this way
		long := strings.Repeat("x", 200)
		box := zero.renderBorderedModal([]styleLine{{text: long}})
		if want := len([]rune(long)) + 6; lipgloss.Width(box) != want {
			t.Fatalf("legacy box width = %d, want %d", lipgloss.Width(box), want)
		}
	})
}

func TestClampModalRowsKeepsFooterAndIndicator(t *testing.T) {
	m := &Model{height: 20}
	rows := []styleLine{{text: "Saved Sessions", highlight: true}}
	for i := 1; i < 38; i++ {
		rows = append(rows, styleLine{text: fmt.Sprintf("row %d", i)})
	}
	rows = append(rows, styleLine{text: "↑↓/jk navigate  enter resume  d delete  esc close"})
	total := len(rows)

	out := m.clampModalRows(rows)
	if len(out) > m.height-4 {
		t.Fatalf("kept %d rows, want <= %d", len(out), m.height-4)
	}
	if last := out[len(out)-1].text; last != rows[total-1].text {
		t.Fatalf("footer row lost: got %q", last)
	}
	indicator := out[len(out)-2].text
	if !strings.Contains(indicator, "more lines") {
		t.Fatalf("missing overflow indicator: %q", indicator)
	}
	wantHidden := total - len(out) + 1
	if !strings.Contains(indicator, fmt.Sprintf("%d more", wantHidden)) {
		t.Fatalf("indicator = %q, want it to report %d hidden lines", indicator, wantHidden)
	}

	t.Run("short list untouched", func(t *testing.T) {
		short := rows[:5]
		if got := m.clampModalRows(short); len(got) != 5 {
			t.Fatalf("clamp rewrote a fitting list: %d rows", len(got))
		}
	})

	t.Run("unset height skips clamp", func(t *testing.T) {
		zero := &Model{}
		if got := zero.clampModalRows(rows); len(got) != total {
			t.Fatalf("zero-height model clamped: %d rows, want %d", len(got), total)
		}
	})
}

func TestHelpModalFitsSmallTerminal(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	m.SetSize(80, 20)
	m.modal = ModalHelp

	modal := m.renderModal()
	if mw, mh := frameDims(modal); mw > 80 || mh > 20-2 {
		t.Fatalf("help modal %dx%d exceeds budget 78x18", mw, mh)
	}
	overlay := renderModalOverlay(modal, m.width, m.height)
	if ow, oh := frameDims(overlay); ow != 80 || oh != 20 {
		t.Fatalf("overlay %dx%d, want exactly 80x20 (Place must fill, not pass through)", ow, oh)
	}
	if last := stripANSI(modal); !strings.Contains(last, "any key to close") {
		t.Fatal("help footer hint lost to clamping")
	}
}

func TestSetSizeFrameNeverExceedsShortTerminal(t *testing.T) {
	for _, h := range []int{24, 10, 9, 8, 7, 6, 5, 4} {
		t.Run(fmt.Sprintf("%d rows", h), func(t *testing.T) {
			m := dragModel(t)
			m.sidebarVisible = false
			m.SetSize(60, h)
			frame := m.renderMainColumn()
			if _, fh := frameDims(frame); fh > h {
				t.Fatalf("frame is %d lines in a %d-row terminal (inline renderer would scroll)", fh, h)
			}
			if fw, _ := frameDims(frame); fw > 60 {
				t.Fatalf("frame is %d cells, want <= 60", fw)
			}
		})
	}
}

func TestSetSizeRestoresInputBandAfterGrowth(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false

	m.SetSize(60, 6) // too short: the 3-line input band gives way
	if got := m.textarea.Height(); got != 1 {
		t.Fatalf("input band = %d at 6 rows, want 1", got)
	}
	m.SetSize(60, 24) // room again: the preferred band height returns
	if got := m.textarea.Height(); got != 3 {
		t.Fatalf("input band = %d after growing back, want the preferred 3", got)
	}
}

func TestSubagentsModalSummaryRuneSafe(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	// 130 two-byte runes: the old byte-slice produced invalid UTF-8. The
	// terminal is wide enough (content budget 123 >= the 121-rune
	// truncated line + prefix) that ONLY the rune-safety path is under
	// test here — the width clamp has its own tests above.
	m.SetSize(140, 30)
	m.subagents = append(m.subagents, subagentRecord{label: "explore", report: strings.Repeat("é", 130)})
	out := stripANSI(m.renderSubagentsModal())
	if !utf8.ValidString(out) {
		t.Fatal("modal output contains invalid UTF-8 (byte-sliced multibyte rune)")
	}
	if !strings.Contains(out, "…") {
		t.Fatal("truncated summary missing ellipsis marker")
	}
}
