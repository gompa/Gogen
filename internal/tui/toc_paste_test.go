package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Regression: pasteCell splices segments into frame rows whose SGR state
// can be open mid-row (styled continuation lines, diff colors). The spliced
// segment must render as authored — an unstyled pasted border previously
// inherited the transcript text's color at the paste point — and the cells
// after the segment must keep their style.

func TestPasteCellIsolatesOpenStyle(t *testing.T) {
	t.Run("unstyled segment does not inherit the open style", func(t *testing.T) {
		lines := []string{"\x1b[36m" + strings.Repeat("a", 20) + "\x1b[0m" + strings.Repeat("b", 5)}
		pasteCell(&lines, 0, 10, "XY")
		got := lines[0]
		if got := ansi.StringWidth(got); got != 25 {
			t.Fatalf("paste changed row width: 25 -> %d\n%q", got, lines[0])
		}
		if !strings.Contains(got, ansi.ResetStyle+"XY") {
			t.Fatalf("pasted segment must start with a terminated style:\n%q", got)
		}
		// The cells after the segment keep the row's open style (the
		// re-open may duplicate the sequence Cut already re-emits —
		// idempotent, so only its presence is asserted).
		if !strings.Contains(got, "XY\x1b[36m") {
			t.Fatalf("suffix cells must re-open the style that was active at the right cut:\n%q", got)
		}
		if !strings.HasSuffix(ansi.Strip(got), "bbbbb") {
			t.Fatalf("suffix cells lost:\n%q", got)
		}
	})

	t.Run("plain row is spliced without extra sequences", func(t *testing.T) {
		lines := []string{"abcdef"}
		pasteCell(&lines, 0, 2, "\x1b[31mR\x1b[0m")
		if got, want := lines[0], "ab\x1b[31mR\x1b[0mdef"; got != want {
			t.Fatalf("paste on a plain row changed bytes:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("row ends closed after replacing its last cell", func(t *testing.T) {
		lines := []string{"\x1b[32m" + strings.Repeat("a", 20) + "\x1b[0m"}
		pasteCell(&lines, 0, 19, "\x1b[36m●\x1b[0m")
		got := lines[0]
		if got := ansi.StringWidth(got); got != 20 {
			t.Fatalf("paste changed row width: 20 -> %d\n%q", got, lines[0])
		}
		if open := openSGRAt(got, ansi.StringWidth(got)); open != "" {
			t.Fatalf("composed row must end closed (SGR carries across rows): open=%q\n%q", open, got)
		}
		if strings.Contains(got, "\x1b[32m●") {
			t.Fatalf("dot inherited the row's open style:\n%q", got)
		}
	})
}

// Regression: the TOC hover tooltip's box borders were unstyled, so pasting
// them into a mid-styled transcript row rendered the border in the
// transcript's color. The box rows now carry their own dim border style,
// and the paste must never reach past the viewport rows (a too-tall box
// on a short terminal used to spill over the divider, composer, and status
// bar).
func TestTocTooltipPaste(t *testing.T) {
	m := newSidebarTestModel(100)
	m.viewport.Height = 20
	viewportRows := 20
	frameRows := viewportRows + 4 // divider + 2 input rows + status bar
	// Cyan left open across every transcript row (a wrapped styled line's
	// continuation): the tooltip border must not inherit it.
	frame := make([]string, frameRows)
	for i := range frame {
		frame[i] = "\x1b[36m" + strings.Repeat("x", 99) + "\x1b[0m "
	}
	m.tocHover = true
	m.tocAnchors = []tocAnchor{{line: 3, text: "a prompt line"}}
	m.tocPreview = 0

	out := m.applyTocOverlay(strings.Join(frame, "\n"))
	lines := strings.Split(out, "\n")
	if len(lines) != frameRows {
		t.Fatalf("overlay changed the frame's row count: %d -> %d", frameRows, len(lines))
	}

	found := 0
	for i := 0; i < viewportRows; i++ {
		if got := ansi.StringWidth(lines[i]); got != 100 {
			t.Fatalf("row %d width changed: 100 -> %d", i, got)
		}
		if strings.Contains(ansi.Strip(lines[i]), "╭") {
			found++
			// The top border must be self-styled dim AND isolated from the
			// row's open style.
			if !strings.Contains(lines[i], ansi.ResetStyle+ansiDimOn+"╭") {
				t.Fatalf("tooltip border not isolated/self-styled:\n%q", lines[i])
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one tooltip top border, got %d", found)
	}
	for i := viewportRows; i < frameRows; i++ {
		if lines[i] != frame[i] {
			t.Fatalf("overlay spilled past the viewport into row %d:\n got %q\nwant %q", i, lines[i], frame[i])
		}
	}
}

// Regression: a tooltip box taller than the viewport must be skipped, not
// clamped to row 0 (which pasted its lower rows over the divider and the
// composer on short terminals).
func TestTocTooltipSkippedWhenTallerThanViewport(t *testing.T) {
	m := newSidebarTestModel(100)
	m.viewport.Height = 2
	frameRows := 6
	frame := make([]string, frameRows)
	for i := range frame {
		frame[i] = strings.Repeat("x", 100)
	}
	m.tocHover = true
	m.tocAnchors = []tocAnchor{{line: 1, text: "a prompt line"}}
	m.tocPreview = 0

	out := m.applyTocOverlay(strings.Join(frame, "\n"))
	if strings.Contains(ansi.Strip(out), "╭") {
		t.Fatalf("too-tall tooltip must be skipped entirely:\n%s", ansi.Strip(out))
	}
	// The single-cell dot is confined to the viewport rows; every frame row
	// below the viewport must be untouched.
	for i, line := range strings.Split(out, "\n") {
		if i < m.viewport.Height {
			continue
		}
		if line != frame[i] {
			t.Fatalf("row %d changed:\n got %q\nwant %q", i, line, frame[i])
		}
	}
}

// Regression: the clamped 6th tooltip line had "…" appended to a
// full-width wrapped line, rendering that row one cell wider than the box
// border (the pasted row overhung the box's right edge). The ellipsis now
// fits the row's padding budget; every box row is exactly w cells.
func TestTocTooltipBoxRowsExactWidth(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"clamped long text", strings.Repeat("word ", 100)},
		{"short text", "short prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range []int{12, 20, 40} {
				box := buildTocTooltipBox(tc.text, 0, w)
				if len(box) > 9 {
					t.Fatalf("box grew past the 6-line clamp: %d rows", len(box))
				}
				for i, row := range box {
					if gw := ansi.StringWidth(row); gw != w {
						t.Fatalf("w=%d row %d is %d cells, want %d:\n%q", w, i, gw, w, ansi.Strip(row))
					}
				}
			}
		})
	}
}
