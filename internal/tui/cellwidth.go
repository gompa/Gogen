package tui

import (
	"bytes"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// cellWidth returns the number of terminal cells s occupies under the SAME
// width method Bubble Tea's renderer uses, which is ansi.WcWidth: ultraviolet's
// screen buffer is created with that method (NewScreenBuffer) and only
// switches to ansi.GraphemeWidth when the terminal reports Unicode core mode
// 2027 as already set (bubbletea tea.go, on the DECRQM reply). Mode 2027 is
// off by default, so WcWidth is what actually paints the frame.
//
// The TUI must measure the same way it is measured. ansi.StringWidth (and
// lipgloss.Width, and lipgloss.Wrap) all use GraphemeWidth, which disagrees
// with the renderer on multi-codepoint grapheme clusters — a ZWJ emoji
// sequence ("👨‍👩‍👧‍👦") is 2 cells by grapheme but 8 by wcwidth, and a skin-tone
// modifier ("👍🏽") is 2 vs 4. Under-reserving that difference shifts whatever
// follows: a sidebar's right border lands short of its column, the status
// bar's middle gap opens, a prompt-rail tooltip border drifts left.
//
// Plain emoji, CJK and ASCII are unaffected — the two methods agree there —
// so this only changes behaviour for the clusters that were actually broken.
//
// A row whose padding lipgloss computes itself (StatusBarStyle.Width, the
// viewport's padding) still pads by GraphemeWidth; that is internal to
// lipgloss v2.0.6 and cannot be redirected from here. Those rows are neither
// helped nor harmed by this change.
func cellWidth(s string) int {
	return ansi.StringWidthWc(s)
}

// blockWidth returns the width of the widest line in a (possibly
// multi-line) block, measured the way the renderer measures.
//
// It exists because lipgloss.Width and ansi.StringWidth do NOT agree on a
// multi-line string: lipgloss.Width reports the widest line, while
// ansi.StringWidth — and ansi.StringWidthWc, hence cellWidth — SUM every
// line (a 60-cell, 11-line modal measures 660 by summation). So cellWidth is
// only a drop-in for lipgloss.Width on a SINGLE line; a block needs this.
func blockWidth(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if lw := cellWidth(line); lw > w {
			w = lw
		}
	}
	return w
}

// cutCells truncates a row to the cell budget the layout reserved for it.
//
// It deliberately stays on ansi.Cut (GraphemeWidth) rather than ansi.CutWc:
// CutWc is broken in x/ansi v0.11.8 — cut() wires truncateLeft = TruncateWc
// for the Wc path, but a left-truncator is TruncateLeftWc, so
// CutWc("abcdef", 1, 3) returns "a" instead of "bc" and CutWc(s, 3, 6)
// returns "abc" instead of "def". That silently corrupted the prompt rail's
// pasteCell and the tooltip rows (whole rows came back at double width).
//
// The truncation sites are safe on GraphemeWidth because the budget they cut
// TO is now measured by cellWidth: reserving the renderer's (Wider) width and
// cutting with the (narrower) grapheme measure can only ever cut a little
// early, never past the reserved column.
func cutCells(s string, left, right int) string {
	return ansi.Cut(s, left, right)
}

// truncateCells cuts s to at most width cells, measured the way the renderer
// measures (cellWidth), keeping escape sequences intact and closing any SGR
// that is still open at the cut so the row's style cannot bleed past it.
//
// ansi.Truncate/CutWc cannot do this job: their early return compares the
// GRAPHEME width (ansi.StringWidth) whatever method was asked for, so a row
// that is 2 cells by grapheme and 8 by wcwidth never takes the truncating
// path at all — exactly the case that needs cutting.
func truncateCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if cellWidth(s) <= width {
		return s
	}
	var b strings.Builder
	var state byte
	cells := 0
	cut := false
	styleOpen := false
	for i := 0; i < len(s); {
		seq, w, n, newState := ansi.DecodeSequenceWc(s[i:], state, nil)
		if w > 0 && cells+w > width {
			cut = true
			break
		}
		if w == 0 {
			if params, ok := sgrSequenceParams(seq); ok {
				styleOpen = params != "" && params != "0"
			}
		}
		b.WriteString(seq)
		cells += w
		state = newState
		if n <= 0 {
			break // defensive: DecodeSequence always consumes ≥ 1 byte
		}
		i += n
	}
	if cut && styleOpen {
		// The cut landed inside an SGR run; close it so the truncation cannot
		// tint the padding that follows (or the next frame row).
		b.WriteString(ansi.ResetStyle)
	}
	return b.String()
}

// isRowBreakingRune reports whether r must not appear inside a single frame
// row: the C0 controls (except ESC, which starts the SGR styling the renderer
// writes) and DEL/C1, all of which either move the cursor (LF, CR, and the
// C1 range, where a terminal may read 0x9B as CSI) or advance it to the next
// tab stop (TAB), making the painted row disagree with the one the frame
// budgeted.
func isRowBreakingRune(r rune) bool {
	if r == '\x1b' {
		return false
	}
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// singleRow makes s safe to render as ONE frame row by replacing the
// characters that move the cursor or change the row's cell accounting with a
// space (see isRowBreakingRune). Escape sequences are left alone: styling is
// the renderer's own.
//
// The status bar and the busy strip are single-row BY CONTRACT — SetSize
// budgets exactly one row for each — and so is every row of the sessions
// panel. Nothing enforced that on the text they interpolate, though, and
// those strings come from outside the layout: an error string from the
// provider, the store, or a tool ("✗ <label>: <err>"), a command's output,
// the working directory, a session label. A value that arrived with an
// embedded newline rendered TWO rows where one was budgeted, so the frame
// came out a row taller than the terminal — the inline renderer scrolled the
// screen by one, its cell diff no longer matched the real screen, and the
// composer and status bar appeared twice (duplicated input band) until
// something forced a full repaint. A TAB is the same failure sideways: the
// frame's own measure counts it as one cell while the terminal expands it to
// the next tab stop and soft-wraps the row.
func singleRow(s string) string {
	clean := true
	for _, r := range s {
		if isRowBreakingRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isRowBreakingRune(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// padCells returns s padded with spaces to exactly width cells (measured the
// renderer's way) and truncated when it would overflow. It is the row-filling
// primitive the viewport and status bar use instead of lipgloss's Width, whose
// padding measures by GRAPHEME width and therefore over-fills any row holding
// a multi-codepoint cluster — pushing the row past the frame edge, where the
// terminal soft-wraps it and desyncs the inline renderer.
func padCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncateCells(s, width)
	if w := cellWidth(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// joinVerticalCells stacks blocks and fills every row to exactly width cells.
//
// It replaces lipgloss.JoinVertical(lipgloss.Left, …), which aligns each row to
// the block's widest line measured by GRAPHEME width: a row holding a
// multi-codepoint cluster (a ZWJ emoji is 2 cells by grapheme and 8 by
// wcwidth) measures narrower than it paints, so the join pads it to the block
// width and pushes it past the frame edge. The terminal then soft-wraps that
// row and every row below shifts — the inline renderer's cell diff desyncs
// from the screen.
func joinVerticalCells(width int, blocks ...string) string {
	var rows []string
	for _, b := range blocks {
		rows = append(rows, strings.Split(b, "\n")...)
	}
	for i, r := range rows {
		rows[i] = padCells(r, width)
	}
	return strings.Join(rows, "\n")
}

// clampFrameRows trims a rendered frame to at most height rows.
//
// It is the last line of defence for the ONE outcome that must never happen:
// an inline frame taller than the terminal scrolls the screen, after which the
// renderer's cell diff no longer matches reality and the frame shows ghost or
// duplicated rows until something forces a full repaint. Every section is
// budgeted so this never fires in practice; a terminal too short for the
// chrome (fewer than 3 rows) is the sole arrangement that cannot fit, and
// there the rows closest to the cursor — the composer and the status bar —
// are the ones worth keeping, so the trim is from the TOP.
func clampFrameRows(frame string, height int) string {
	if height <= 0 {
		return frame
	}
	rows := strings.Split(frame, "\n")
	if len(rows) <= height {
		return frame
	}
	return strings.Join(rows[len(rows)-height:], "\n")
}

// joinHorizontalCells places left beside right (top-aligned), padding every
// row of each side to its own column width in the renderer's measure and the
// block with fewer rows with blank rows. It replaces lipgloss.JoinHorizontal
// for the sidebar, for the same grapheme-vs-wcwidth reason as
// joinVerticalCells: a session label holding a ZWJ cluster makes the panel row
// measure narrower than it paints, and lipgloss's padding then widens the
// joined frame past the terminal.
func joinHorizontalCells(left string, leftWidth int, right string, rightWidth int) string {
	leftRows := strings.Split(left, "\n")
	rightRows := strings.Split(right, "\n")
	rows := max(len(leftRows), len(rightRows))
	out := make([]string, rows)
	for i := range out {
		var l, r string
		if i < len(leftRows) {
			l = padCells(leftRows[i], leftWidth)
		} else {
			l = padCells("", leftWidth)
		}
		if i < len(rightRows) {
			r = padCells(rightRows[i], rightWidth)
		} else {
			r = padCells("", rightWidth)
		}
		out[i] = l + r
	}
	return strings.Join(out, "\n")
}

// wrapCells is lipgloss.Wrap measured the way the renderer measures, for the
// same reason as cellWidth. It mirrors lipgloss.Wrap exactly — ansi.Wrap to
// break the line, then a WrapWriter pass to re-open any SGR style that is
// still active at each inserted newline — with the Wc-width wrapper swapped
// in. Both halves matter: the WrapWriter pass is what keeps a styled chat
// line styled across its wrap points (see wrapLine).
func wrapCells(s string, width int) string {
	if width < 1 {
		return s
	}
	var buf bytes.Buffer
	w := lipgloss.NewWrapWriter(&buf)
	defer w.Close() //nolint:errcheck
	_, _ = io.WriteString(w, ansi.WrapWc(s, width, ""))
	return buf.String()
}
