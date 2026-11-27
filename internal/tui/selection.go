package tui

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

// stripANSI removes ANSI escape sequences from a string. Uses ansi.Strip
// (parser-based) rather than a regex so non-SGR escapes (OSC, etc.) are
// removed as well.
func stripANSI(s string) string {
	return ansi.Strip(s)
}

// Selection coordinates are rows of the viewport's wrapped content. The rows
// themselves live in exactly one place (the viewport); the PLAIN text of a row
// — ANSI stripped — is derived on demand, for the few rows a mouse event or a
// visible frame actually needs. The previous design kept three extra copies of
// the whole transcript (a joined string, its ANSI-stripped split and its styled
// split) and rebuilt them after every streaming batch.

// plainLine returns row i of the wrapped transcript with ANSI stripped, or ""
// when i is out of range.
func (m *Model) plainLine(i int) string {
	lines := m.viewport.Lines()
	if i < 0 || i >= len(lines) {
		return ""
	}
	return stripANSI(lines[i])
}

// plainLineCount is the number of wrapped transcript rows.
func (m *Model) plainLineCount() int { return len(m.viewport.Lines()) }

// SelectionState tracks the in-progress or finalized text selection.
// StartX/EndX are terminal cell columns (not byte offsets) into the plain line.
type SelectionState struct {
	Active   bool
	Dragging bool // true while the mouse button is held during a drag
	StartX   int  // cell column in plain content (0-based)
	StartY   int  // line in plain content (0-based)
	EndX     int
	EndY     int

	// text caches the plain text of a FINALIZED selection (captured by
	// finalizeSelection). A rebuild re-anchors by it instead of re-slicing
	// the covered rows on every streaming batch: with a large selection and
	// a tool line streaming above it, that slice would run tens of times a
	// second. It is an anchor hint, never the copy source — the copy always
	// reads the rows the highlight is drawn on.
	//
	// The anchor is the covered rows joined by '\n' with EVERY row emitting
	// its separator, blank rows included (selectionAnchorText), because
	// reanchorSelection searches the new rows' joined text: the anchor must
	// have the same row layout. The copy output (getSelectedText) drops blank
	// rows instead, and the two differ by exactly those separators — so
	// anchoring on the copy text made a selection spanning a blank row search
	// "…\n…" inside a window holding "…\n\n…", never match, and drop the
	// highlight while the text was still on screen.
	text string
}

// mouseEventKind identifies which bubbletea v2 mouse message a mouseEvent
// was normalized from. (v1 carried everything in one MouseMsg with an
// Action field; v2 splits events into distinct message types.)
type mouseEventKind uint8

const (
	mousePress mouseEventKind = iota
	mouseRelease
	mouseMotion
	mouseWheelEvent
)

// mouseEvent normalizes bubbletea v2's typed mouse messages into the single
// shape the selection logic needs (the moral equivalent of v1's MouseMsg).
type mouseEvent struct {
	x, y   int
	button tea.MouseButton
	kind   mouseEventKind
}

// normalizeMouseEvent converts a typed bubbletea v2 mouse message into a
// mouseEvent. Returns ok=false for non-mouse messages.
func normalizeMouseEvent(msg tea.Msg) (mouseEvent, bool) {
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mousePress}, true
	case tea.MouseReleaseMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mouseRelease}, true
	case tea.MouseMotionMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mouseMotion}, true
	case tea.MouseWheelMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mouseWheelEvent}, true
	}
	return mouseEvent{}, false
}

// handleMouseSelection processes mouse events for text selection.
// Returns true if the event was consumed (selection handled), false if
// it should be passed through to the viewport for wheel scrolling.
func (m *Model) handleMouseSelection(ev mouseEvent) bool {
	// Block wheel only while dragging so content coordinates stay stable.
	// After release, scrolling is allowed and the highlight tracks content.
	if ev.kind == mouseWheelEvent {
		if m.selection != nil && m.selection.Dragging {
			return true
		}
		return false
	}

	vpHeight := m.viewport.Height
	if m.selection != nil && m.selection.Active {
		return m.handleActiveSelectionMouse(ev, vpHeight)
	}
	return m.startSelectionAt(ev, vpHeight)
}

// handleActiveSelectionMouse processes mouse events while a selection is
// active: drag updates the end point, release finalizes it, right-click
// clears, and left-press restarts. Returns true when the event was consumed.
func (m *Model) handleActiveSelectionMouse(ev mouseEvent, vpHeight int) bool {
	switch {
	case m.selection.Dragging && ev.button == tea.MouseLeft && ev.kind == mouseMotion:
		x, y := m.mouseToContent(ev.x, ev.y)
		if x >= 0 && y >= 0 {
			m.selection.EndX = x
			m.selection.EndY = y
		}
		return true
	case m.selection.Dragging && ev.kind == mouseRelease:
		m.finalizeSelection()
		return true
	case m.selection.Dragging && ev.button == tea.MouseLeft && ev.kind == mousePress:
		// A left PRESS while the drag is still marked live means the drag's
		// RELEASE never arrived: the pointer left the window mid-drag, the
		// terminal dropped the event, or a modal was up when it happened.
		// Ignoring the press left Dragging set indefinitely — the wheel
		// stayed locked and the next drag kept the stale anchor, so the first
		// selection after a lost release copied the wrong range (or nothing).
		// A press is a fresh anchor, so restart here instead.
		m.clearSelection()
		return m.startSelectionAt(ev, vpHeight)
	case ev.button == tea.MouseRight && (ev.kind == mousePress || ev.kind == mouseRelease):
		m.clearSelection()
		m.statusMsg = ""
		return true
	case !m.selection.Dragging && ev.button == tea.MouseLeft &&
		ev.kind == mousePress && ev.y >= 0 && ev.y < vpHeight:
		// Replace the existing selection; start logic below the if handles it.
		m.clearSelection()
		return m.startSelectionAt(ev, vpHeight)
	case m.selection.Dragging:
		// Ignore other events while dragging (e.g. button-less motion that
		// some terminals emit). Do NOT clear — that was wiping the
		// selection before the user could copy.
		return true
	default:
		return false
	}
}

// startSelectionAt begins a new selection on a left-press inside the
// viewport. Returns true when the event was consumed.
func (m *Model) startSelectionAt(ev mouseEvent, vpHeight int) bool {
	if ev.button == tea.MouseLeft && ev.kind == mousePress &&
		ev.y >= 0 && ev.y < vpHeight {
		x, y := m.mouseToContent(ev.x, ev.y)
		m.statusMsg = ""
		if x >= 0 && y >= 0 {
			m.selectionYOff = m.viewport.YOffset
			m.selection = &SelectionState{
				Active:   true,
				Dragging: true,
				StartX:   x,
				StartY:   y,
				EndX:     x,
				EndY:     y,
			}
		}
		return true
	}
	return false
}

// finalizeSelection ends a drag: unlock scrolling, report the selected
// character count (or clear when nothing was actually selected).
func (m *Model) finalizeSelection() {
	m.selection.Dragging = false
	m.selectionYOff = -1 // unlock scroll; coords are content-absolute
	text := m.getSelectedText()
	if text == "" {
		m.clearSelection()
		m.statusMsg = ""
	} else {
		// The anchor is the row-joined form, not the copy form: a blank row
		// inside the selection must keep its separator or reanchorSelection
		// searches a string the new rows can never contain.
		m.selection.text = m.selectionAnchorText()
		m.statusMsg = fmt.Sprintf("Selected %d chars — ctrl+shift+c to copy", utf8.RuneCountInString(text))
	}
}

// matchesCopyChord reports whether msg is the copy chord.
//
// It is not just key.Matches: that compares the binding against Key.String(),
// which returns Key.Text verbatim whenever the terminal filled it in — a chord
// delivered as {Code:'c', ShiftedCode:'C', Text:"C", Mod: ctrl|shift}
// stringifies as "C" and silently missed the binding (Key.Keystroke() spells
// the modifiers out as "ctrl+shift+c"). Matching the keystroke form as well
// keeps the chord working whatever shape the terminal reports, without
// widening the binding to any other key.
//
// The keystroke comparison is case-insensitive because the letter's case is
// not part of the chord: terminals that report the SHIFTED code as the main
// code (kitty's "C" alternate key used alone, xterm's modifyOtherKeys with the
// shifted code point — CSI 67;6u and CSI 27;6;67~) spell it "ctrl+shift+C",
// and a case-sensitive compare made the copy key dead on those terminals while
// the highlight was on screen. Folding only the case cannot steal another
// chord: ctrl+c (cancel) has no shift bit and is matched before this.
func (m *Model) matchesCopyChord(msg tea.KeyPressMsg) bool {
	if key.Matches(msg, m.keys.CopySelection) {
		return true
	}
	stroke := msg.Keystroke()
	for _, want := range m.keys.CopySelection.Keys() {
		if stroke == want || strings.EqualFold(stroke, want) {
			return true
		}
	}
	return false
}

// copySelection copies the current selection to the clipboard. Returns true
// if a selection was present, plus a tea.Cmd when copying had to fall back
// to the terminal (OSC52 via tea.SetClipboard) — the caller must run it.
//
// The local-clipboard path clears the selection on success; the OSC52 path
// keeps it, because OSC52 is fire-and-forget (no delivery ack) and a retry
// should be one keystroke away.
func (m *Model) copySelection() (bool, tea.Cmd) {
	text := m.getSelectedText()
	if text == "" {
		// Nothing selected: say so instead of silently doing nothing. This is
		// the state a copy-key press lands in when the selection was dropped
		// under a rebuild (see setViewportContent), and the old empty branch
		// made that indistinguishable from a broken keybinding.
		m.statusMsg = "No selection — drag to select text, then ctrl+shift+c"
		return false, nil
	}
	return true, m.writeSelection(text)
}

// writeClipboard is the local-clipboard writer. It is a package variable so
// tests can stub it: the real one shells out to wl-copy/xclip/xsel, which must
// not run (and must not touch the developer's clipboard) during `go test`.
var writeClipboard = clipboard.WriteAll

// writeSelection writes text to the clipboard and reports the outcome on the
// status line, returning the OSC52 fallback cmd when the local clipboard is
// unavailable.
func (m *Model) writeSelection(text string) tea.Cmd {
	n := utf8.RuneCountInString(text)
	if err := writeClipboard(text); err == nil {
		m.statusMsg = fmt.Sprintf("✓ Copied %d chars to clipboard", n)
		m.clearSelection()
		return nil
	}
	// Local clipboard unavailable (headless, SSH without wl-copy/xclip/xsel,
	// a container without the X socket…): fall back to OSC52 through
	// bubbletea — most modern terminals honor it even over SSH. OSC52 has no
	// delivery ack and plenty of terminals drop it (disabled by policy, tmux
	// without passthrough, a payload over the terminal's size cap), so the
	// message says which path was taken instead of claiming success: a
	// "copied" line followed by an empty paste is exactly the confusing
	// report this status line exists to rule out.
	m.statusMsg = fmt.Sprintf("Sent %d chars to the terminal clipboard (OSC52) — it may be ignored", n)
	return tea.SetClipboard(text)
}

// runeWidth returns the terminal cell width of r: 2 for wide (CJK/emoji), 1 for
// ordinary, and 0 for zero-width runes — combining marks, ZWJ, variation
// selectors. They must NOT count as their own cell: a selection coordinate is a
// CELL column, and treating "e" + U+0301 as two cells made every cell after it
// one column off (the highlight landed on the wrong characters and the copied
// text was cut short).
func runeWidth(r rune) int {
	if w := cellWidth(string(r)); w > 0 {
		return w
	}
	return 0
}

// sliceByCells returns the substring of s covering terminal cells [start, end).
func sliceByCells(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end <= start {
		return ""
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		rw := runeWidth(r)
		if rw == 0 {
			// Zero-width: it belongs to the cell of the character before it.
			if col-1 >= start && col-1 < end {
				b.WriteRune(r)
			}
			continue
		}
		next := col + rw
		if next > start && col < end {
			b.WriteRune(r)
		}
		col = next
		// Stop only PAST the range: a zero-width rune at col == end belongs to
		// the last included cell, so it must still be visited.
		if col > end {
			break
		}
	}
	return b.String()
}

// cellsToRuneRange maps a half-open cell range to a half-open rune range in s.
func cellsToRuneRange(s string, startCell, endCell int) (startRi, endRi int) {
	if endCell <= startCell {
		return 0, 0
	}
	col := 0
	ri := 0
	startRi = -1
	for _, r := range s {
		rw := runeWidth(r)
		if rw == 0 {
			if col-1 >= startCell && col-1 < endCell {
				if startRi < 0 {
					startRi = ri
				}
				endRi = ri + 1
			}
			ri++
			continue
		}
		next := col + rw
		if next > startCell && col < endCell {
			if startRi < 0 {
				startRi = ri
			}
			endRi = ri + 1
		}
		col = next
		ri++
		if col > endCell {
			// Past the range; a zero-width rune at the boundary was handled
			// above (it belongs to the last included cell).
			break
		}
	}
	if startRi < 0 {
		return 0, 0
	}
	return startRi, endRi
}

// mouseToContent converts terminal-relative mouse coordinates to
// content coordinates (line and cell column in the plain wrapped content).
// Coordinates outside the viewport's visible band are clamped into it: a drag
// routinely leaves the transcript (down over the divider, composer and status
// bar, or the blank rows a short transcript leaves at the bottom of the band),
// and the mapping must land on the nearest line that is actually on screen.
// Returns (-1, -1) when there is nothing to address.
func (m *Model) mouseToContent(mouseX, mouseY int) (int, int) {
	// Chat column starts right of the sessions panel; presses on the panel
	// itself never select chat text.
	mouseX -= m.sidebarOffsetX()
	if mouseX < 0 {
		return -1, -1
	}
	lineCount := m.plainLineCount()
	if lineCount == 0 {
		return -1, -1
	}
	// Clamp the ROW to the viewport's visible band before it is mapped to a
	// content line. Without this a drag that ended on the chrome rows below
	// the transcript named a content line that the reader cannot see (the
	// transcript continues under the divider, composer and status bar), so a
	// selection near the bottom of the screen silently covered lines that
	// were never highlighted, and a drag that ended below the last line was
	// dropped outright — the final motion event carried no valid point, and
	// finalizeSelection then threw the whole selection away as empty.
	if mouseY < 0 {
		mouseY = 0
	}
	if h := m.viewport.Height; h > 0 && mouseY >= h {
		mouseY = h - 1
	}
	// Account for viewport scroll position. While dragging, freeze to the
	// YOffset captured at press so motion coordinates stay stable.
	contentY := mouseY + m.viewport.YOffset
	if m.selection != nil && m.selection.Dragging && m.selectionYOff >= 0 {
		contentY = mouseY + m.selectionYOff
	}
	// Clamp to the transcript itself: rows past the last line (a transcript
	// shorter than the viewport leaves blank rows) address the end of the
	// text, like a caret clamped to the end of a document.
	if contentY >= lineCount {
		contentY = lineCount - 1
	}
	if contentY < 0 {
		contentY = 0
	}

	// Account for left padding (ViewportStyle has PaddingLeft(1)) and the
	// horizontal scroll offset: with xOffset > 0 the first visible content
	// cell IS xOffset, so the content column is shifted right by it (the
	// same mapping the render's [xOffset, xOffset+w) window implies).
	leftPad := m.viewport.Style.GetPaddingLeft()
	contentX := mouseX - leftPad + m.viewport.xOffset
	if contentX < 0 {
		contentX = 0
	}

	line := m.plainLine(contentY)
	if max := cellWidth(line); contentX > max {
		contentX = max
	}

	return contentX, contentY
}

// getSelectedText returns the plain text currently selected, or "" if
// nothing is selected. StartX/EndX are cell columns. Rows that contribute no
// picked cell (a blank transcript row, a row whose selected cells are all
// blank) emit no separator: the copy is the picked characters, not the layout
// around them.
func (m *Model) getSelectedText() string { return m.selectionText(false) }

// selectionAnchorText returns the same selection as the exact row-joined text
// it covers: every covered row emits its '\n' separator even when it
// contributes no picked cell, so the result has one line per row and can be
// compared against the joined text reanchorSelection builds from the new rows
// (see SelectionState.text).
func (m *Model) selectionAnchorText() string { return m.selectionText(true) }

// selectionText builds the selection's plain text. joinBlankRows selects the
// anchor form (one line per covered row) over the copy form (blank rows
// skipped).
func (m *Model) selectionText(joinBlankRows bool) string {
	if m.selection == nil || m.plainLineCount() == 0 {
		return ""
	}

	startY, endY := m.selection.StartY, m.selection.EndY
	startX, endX := m.selection.StartX, m.selection.EndX

	// Normalize: ensure start comes before end
	if startY > endY || (startY == endY && startX > endX) {
		startY, endY = endY, startY
		startX, endX = endX, startX
	}

	var b strings.Builder
	for y := startY; y <= endY; y++ {
		if y >= m.plainLineCount() {
			break
		}
		line := m.plainLine(y)
		ls := 0
		le := cellWidth(line)
		if y == startY {
			ls = startX
		}
		if y == endY {
			le = endX
		}
		chunk := sliceByCells(line, ls, le)
		if joinBlankRows {
			if y > startY {
				b.WriteByte('\n')
			}
			b.WriteString(chunk)
			continue
		}
		if chunk != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(chunk)
		}
	}
	return b.String()
}

// clearSelection removes any active selection and resets state.
func (m *Model) clearSelection() {
	m.selection = nil
	m.selectionYOff = -1
}

// reanchorWindow bounds the row window reanchorSelection scans on either side
// of the selection. Rows only move by an insert/delete above or by a re-wrap,
// so a match that is further away than this is not the text the user picked.
const reanchorWindow = 64

// selectionAfterRebuild reports the selection to KEEP when the viewport's rows
// have just been replaced by newRows: the same selection when every row it
// covers still holds the same plain text, or — when a row's text changed — the
// same TEXT re-anchored to the rows that now hold it (reanchorSelection). Nil
// when neither works, i.e. the text is gone or ambiguous.
//
// The exact comparison is what separates "the transcript grew, your selection
// is still exactly where you made it" — an append, a round boundary, a
// redundant re-wrap at the same width — from "the rows moved under you": a
// compaction, a session or model switch, or a resize that re-wrapped the text.
// Plain text (not the styled row) is compared: a style-only re-render must not
// drop a selection, while a row whose TEXT changed is no longer what the user
// selected.
//
// Must be called with the OLD rows still published (before SetContentLines).
func (m *Model) selectionAfterRebuild(newRows []string) *SelectionState {
	s := m.selection
	if s == nil || !s.Active {
		return nil
	}
	lo, hi := s.StartY, s.EndY
	if lo > hi {
		lo, hi = hi, lo
	}
	old := m.viewport.Lines()
	if lo < 0 || hi >= len(old) {
		return nil
	}
	same := hi < len(newRows)
	for y := lo; same && y <= hi; y++ {
		if stripANSI(old[y]) != stripANSI(newRows[y]) {
			same = false
		}
	}
	if same {
		return s
	}
	// A live drag cannot be re-anchored: its end point is driven by the
	// pointer, and the frozen scroll offset it maps through belongs to the
	// old rows.
	if s.Dragging {
		return nil
	}
	return m.reanchorSelection(newRows)
}

// reanchorSelection maps the current (finalized) selection onto newRows by its
// TEXT: the rows it covered changed — a rewritten streaming line (a tool call's
// growing args, a patch diff), a row inserted or dropped above it, or a resize
// that re-wrapped the paragraph — but the characters the user picked are still
// on screen, so the highlight and the copy can follow them instead of being
// dropped. That silent drop is what made ctrl+shift+c "sometimes do nothing"
// while the text was still visible.
//
// Conservative by design: the text must occur EXACTLY ONCE inside a bounded
// window around the old position. A repeated snippet (a common log line, an
// empty selection) is left alone — guessing which copy the user meant would
// highlight the wrong line, and a wrong copy is worse than a reported drop.
//
// Must be called with the OLD rows still published (before SetContentLines).
func (m *Model) reanchorSelection(newRows []string) *SelectionState {
	s := m.selection
	text := s.text
	if text == "" {
		// No cached anchor (a selection finalized by an older path): build the
		// row-joined form, never the copy form — see SelectionState.text.
		text = m.selectionAnchorText()
	}
	if text == "" || len(newRows) == 0 {
		return nil
	}
	lo, hi := s.StartY, s.EndY
	if lo > hi {
		lo, hi = hi, lo
	}
	from := max(0, lo-reanchorWindow)
	to := min(len(newRows)-1, hi+reanchorWindow)
	if to < from {
		return nil
	}

	// Join the window's plain rows, remembering where each row starts so a
	// byte offset in the joined string can be mapped back to (row, cell).
	rows := make([]string, 0, to-from+1)
	starts := make([]int, 0, to-from+1)
	var joined strings.Builder
	for y := from; y <= to; y++ {
		if y > from {
			joined.WriteByte('\n')
		}
		starts = append(starts, joined.Len())
		plain := stripANSI(newRows[y])
		rows = append(rows, plain)
		joined.WriteString(plain)
	}
	content := joined.String()

	at := strings.Index(content, text)
	if at < 0 {
		return nil
	}
	// Unique match only: a second occurrence makes the anchor a guess.
	if strings.Contains(content[at+1:], text) {
		return nil
	}

	startY, startX, ok := reanchorPoint(rows, starts, content, at)
	if !ok {
		return nil
	}
	endY, endX, ok := reanchorPoint(rows, starts, content, at+len(text))
	if !ok {
		return nil
	}
	out := *s
	out.StartY, out.StartX = from+startY, startX
	out.EndY, out.EndX = from+endY, endX
	return &out
}

// reanchorPoint maps a byte offset in the joined window text back to a row
// index within the window and a CELL column in that row. An offset sitting on
// a row separator belongs to the end of the row before it (the offset just
// past the last character of that row), which is where a selection ending at a
// row boundary ends.
func reanchorPoint(rows []string, starts []int, content string, off int) (row, cell int, ok bool) {
	if off < 0 || off > len(content) {
		return 0, 0, false
	}
	// Last row whose start is <= off: the row containing the offset.
	i := sort.Search(len(starts), func(i int) bool { return starts[i] > off }) - 1
	if i < 0 {
		i = 0
	}
	row = i
	within := off - starts[i]
	if within > len(rows[i]) {
		// The offset sits on the separator after this row.
		within = len(rows[i])
	}
	return row, cellWidth(rows[i][:within]), true
}

// renderViewportWithSelection renders the viewport content with selection
// highlighting applied. It replicates viewport.View() but injects reverse-video
// ANSI codes on selected text to preserve all original styling while adding
// the selection highlight.
func (m *Model) renderViewportWithSelection() string {
	w := m.viewport.Width
	h := m.viewport.Height
	if sw := m.viewport.Style.GetWidth(); sw != 0 {
		if sw < w {
			w = sw
		}
	}
	if sh := m.viewport.Style.GetHeight(); sh != 0 {
		if sh < h {
			h = sh
		}
	}
	contentWidth := w - m.viewport.Style.GetHorizontalFrameSize()
	contentHeight := h - m.viewport.Style.GetVerticalFrameSize()

	// Freeze scroll offset only while dragging; after release follow viewport.
	yOff := m.viewport.YOffset
	if m.selection != nil && m.selection.Dragging && m.selectionYOff >= 0 {
		yOff = m.selectionYOff
	}
	// The viewport already holds the styled wrapped rows: the selection render
	// reads them directly instead of keeping its own joined+split copy.
	styledLines := m.viewport.Lines()

	// Normalize selection range
	selSY, selEY := m.selection.StartY, m.selection.EndY
	selSX, selEX := m.selection.StartX, m.selection.EndX
	if selSY > selEY || (selSY == selEY && selSX > selEX) {
		selSY, selEY = selEY, selSY
		selSX, selEX = selEX, selSX
	}

	// Match viewport.visibleLines' horizontal window EXACTLY: it cuts every
	// visible line to [xOffset, xOffset+contentWidth) when the viewport is
	// scrolled or when any content line overflows — the same
	// truncate-don't-rewrap rule that keeps lipgloss MaxWidth from
	// soft-wrapping long lines into extra rows and shifting everything
	// below (the selection jump bug). The selection render must show the
	// same cells as the normal render, or the transcript visibly shifts the
	// moment a drag starts.
	cutX := m.viewport.xOffset
	cutAll := cutX > 0 || m.viewport.longestLineWidth > contentWidth

	var lines []string
	for i := 0; i < contentHeight; i++ {
		ci := yOff + i
		if ci < len(styledLines) {
			line := styledLines[ci]
			if ci >= selSY && ci <= selEY {
				plain := stripANSI(line)
				lineWidth := cellWidth(plain)
				hs := 0
				he := lineWidth
				if ci == selSY {
					hs = selSX
				}
				if ci == selEY {
					he = selEX
				}
				if hs > lineWidth {
					hs = lineWidth
				}
				if he > lineWidth {
					he = lineWidth
				}
				if hs < he {
					line = highlightPlainRange(line, plain, hs, he)
				}
			}
			if cutAll && contentWidth > 0 {
				line = cutCells(line, cutX, cutX+contentWidth)
			}
			lines = append(lines, line)
		} else {
			lines = append(lines, "")
		}
	}

	// Pad with fitRows, exactly as Viewport.View does — not with a lipgloss
	// style. It is the same "exactly height rows of width cells, truncate
	// don't re-wrap" contract the normal render uses, measured in wcwidth (so
	// the selection frame cannot disagree with the unselected one about row
	// widths), and it skips a full lipgloss Render pass over the whole screen
	// on every frame of a drag.
	return m.viewport.Style.
		UnsetWidth().UnsetHeight().
		Render(fitRows(lines, contentWidth, contentHeight))
}

// highlightPlainRange inserts reverse-video ANSI codes (\x1b[7m ... \x1b[27m)
// into a styled string over terminal cells [start, end) of the plain text.
// It preserves all existing ANSI styling while adding the selection highlight.
func highlightPlainRange(styled, plain string, start, end int) string {
	startRi, endRi := cellsToRuneRange(plain, start, end)
	if startRi >= endRi {
		return styled
	}

	styledStart := mapPlainToStyled(startRi, styled)
	styledEnd := mapPlainToStyled(endRi, styled)

	if styledStart >= styledEnd {
		return styled
	}

	return styled[:styledStart] + "\x1b[7m" + styled[styledStart:styledEnd] + "\x1b[27m" + styled[styledEnd:]
}

// mapPlainToStyled maps a plain-text rune index to the corresponding
// byte offset in an ANSI-styled string. It skips over ANSI escape sequences
// and handles UTF-8 multi-byte characters.
func mapPlainToStyled(plainIdx int, styled string) int {
	plainPos := 0
	for i := 0; i < len(styled); {
		// Skip ANSI escape sequences. CSI (ESC [) ends at its final byte; OSC
		// (ESC ]) ends at BEL or ST (ESC \). Skipping only CSI counted an OSC
		// payload as plain text, shifting every mapping after a hyperlink.
		if styled[i] == '\x1b' && i+1 < len(styled) {
			switch styled[i+1] {
			case '[':
				end := i + 2
				for end < len(styled) {
					c := styled[end]
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
						end++
						break
					}
					end++
				}
				i = end
				continue
			case ']':
				end := i + 2
				for end < len(styled) {
					if styled[end] == '\x07' {
						end++
						break
					}
					if styled[end] == '\x1b' && end+1 < len(styled) && styled[end+1] == '\\' {
						end += 2
						break
					}
					end++
				}
				i = end
				continue
			}
		}
		if plainPos >= plainIdx {
			return i
		}
		_, size := utf8.DecodeRuneInString(styled[i:])
		i += size
		plainPos++
	}
	return len(styled)
}
