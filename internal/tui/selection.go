package tui

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/x/ansi"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// sgrState tracks the SGR attributes active at a point inside a styled
// string, in enough detail to re-emit them at a word-wrap point: foreground
// and background colors (basic, bright, and extended 256/truecolor forms)
// plus the common attribute flags (bold, dim, italic, underline, blink,
// reverse, hidden).
//
// It exists because wrap-point style propagation must know which attributes
// are actually open — matching on a literal reset sequence does not work:
// lipgloss v2 closes styles with a bare "\x1b[m" and property-specific
// resets ("\x1b[22m", "\x1b[39m") instead of v1's "\x1b[0m".
type sgrState struct {
	// ops holds one entry per active attribute, each the raw SGR parameter
	// string that set it ("1" for bold, "38;2;0;170;170" for a truecolor
	// fg), in application order so sequence() reproduces them verbatim.
	ops []string
}

// reset clears every active attribute.
func (st *sgrState) reset() { st.ops = nil }

// hasAttrs reports whether any attribute is active.
func (st *sgrState) hasAttrs() bool { return len(st.ops) > 0 }

// sequence renders the active attributes as a single SGR sequence, or ""
// when nothing is active. The result is safe to prepend to a continuation
// line to restore styling.
func (st *sgrState) sequence() string {
	if len(st.ops) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(st.ops, ";") + "m"
}

// removeOps drops every active op whose class matches class of op code n.
func (st *sgrState) removeOps(class sgrClass) {
	kept := st.ops[:0]
	for _, op := range st.ops {
		if sgrOpClass(op) != class {
			kept = append(kept, op)
		}
	}
	st.ops = kept
}

func (st *sgrState) addOp(op string) { st.ops = append(st.ops, op) }

// sgrClass groups SGR codes into replaceable families: setting a new fg
// replaces the previous fg rather than stacking with it.
type sgrClass uint8

const (
	sgrClassOther   sgrClass = iota // flags (bold, italic, ...)
	sgrClassReset                   // 0 / empty
	sgrClassFlagOff                 // 22, 23, 24, ...
	sgrClassFg                      // 30-37, 38, 90-97, 39
	sgrClassBg                      // 40-47, 48, 100-107, 49
)

// sgrOpClass classifies a raw SGR parameter group (first token decides for
// extended-color forms like "38;5;n").
func sgrOpClass(op string) sgrClass {
	n, err := strconv.Atoi(op)
	if err != nil {
		return sgrClassOther
	}
	switch {
	case n == 0:
		return sgrClassReset
	case n == 39 || (n >= 30 && n <= 37) || (n >= 90 && n <= 97):
		return sgrClassFg
	case n == 49 || (n >= 40 && n <= 47) || (n >= 100 && n <= 107):
		return sgrClassBg
	case n == 38 || n == 48:
		if n == 38 {
			return sgrClassFg
		}
		return sgrClassBg
	case n >= 21 && n <= 29: // attribute-offs: 22 bold/dim off, 23 italic off, ...
		return sgrClassFlagOff
	default:
		return sgrClassOther
	}
}

// flagOffTargets maps attribute-off codes to the on-codes they cancel
// (22 cancels bold "1" and dim "2", 27 cancels reverse "7", etc.).
var flagOffTargets = map[string][]string{
	"22": {"1", "2"},
	"23": {"3"},
	"24": {"4"},
	"25": {"5"},
	"27": {"7"},
	"28": {"8"},
	"29": {"9"},
}

// apply applies one SGR sequence's parameters (the text between "\x1b["
// and "m") to the state.
func (st *sgrState) apply(params string) {
	toks := strings.FieldsFunc(params, func(r rune) bool { return r == ';' })
	if len(toks) == 0 {
		// A bare "\x1b[m" or "\x1b[;m" is a full reset.
		st.reset()
		return
	}
	for i := 0; i < len(toks); i++ {
		op := toks[i]
		n, err := strconv.Atoi(op)
		if err != nil {
			continue // malformed parameter — skip
		}
		switch {
		case n == 0:
			st.reset()
		case n == 38 || n == 48:
			// Extended color: consume its argument tokens so they form one
			// op — "38;5;<n>" (3 extra) or "38;2;<r>;<g>;<b>" (4 extra).
			extra := 0
			if i+1 < len(toks) {
				switch toks[i+1] {
				case "5":
					extra = 2
				case "2":
					extra = 4
				}
			}
			group := op
			for k := 0; k < extra && i+1 < len(toks); k++ {
				i++
				group += ";" + toks[i]
			}
			st.removeOps(sgrClass(n))
			st.addOp(group)
		case n == 39:
			st.removeOps(sgrClassFg) // default fg
		case n == 49:
			st.removeOps(sgrClassBg) // default bg
		case (n >= 30 && n <= 37) || (n >= 90 && n <= 97):
			st.removeOps(sgrClassFg)
			st.addOp(op)
		case (n >= 40 && n <= 47) || (n >= 100 && n <= 107):
			st.removeOps(sgrClassBg)
			st.addOp(op)
		case n >= 21 && n <= 29:
			for _, target := range flagOffTargets[op] {
				st.removeExact(target)
			}
		default:
			// Attribute flags (1 bold, 2 dim, 3 italic, 4 underline, 5 blink,
			// 7 reverse, 8 hidden, 9 strike, plus rarely-used others).
			st.addOp(op)
		}
	}
}

// removeExact removes the op equal to op (used by attribute-off codes).
func (st *sgrState) removeExact(op string) {
	kept := st.ops[:0]
	for _, existing := range st.ops {
		if existing != op {
			kept = append(kept, existing)
		}
	}
	st.ops = kept
}

// applySGRs advances st over all SGR sequences contained in s, leaving it
// holding the attributes active at the end of s. Non-SGR CSI sequences are
// ignored.
func applySGRs(st *sgrState, s string) {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '\x1b' || s[i+1] != '[' {
			continue
		}
		j := i + 2 // past ESC[
		for j < len(s) {
			c := s[j]
			if c == 'm' {
				st.apply(s[i+2 : j])
				break
			}
			// Parameter bytes only; any other final byte ends a non-SGR CSI.
			if (c < '0' || c > '9') && c != ';' && c != ':' {
				break
			}
			j++
		}
		i = j // resume after the consumed sequence
	}
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// truncateRunes truncates s to at most maxRunes runes, returning the original
// string if it is already shorter. This avoids slicing multi-byte UTF-8
// characters mid-rune.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

// ensureWrappedLines lazily computes m.wrappedLines (ANSI‑stripped) when it is
// dirty.  This avoids expensive stripANSI + Split on every streaming token.
func (m *Model) ensureWrappedLines() {
	if !m.wrappedLinesDirty {
		return
	}
	if content := m.wrappedContentString(); content == "" {
		m.wrappedLines = nil
	} else {
		m.wrappedLines = strings.Split(stripANSI(content), "\n")
	}
	m.wrappedLinesDirty = false
}

// ensureStyledLines lazily computes m.styledLines (ANSI‑preserved split)
// when it is dirty. This avoids the expensive Split on every streaming token
// when no text selection is active.
func (m *Model) ensureStyledLines() {
	if !m.styledLinesDirty {
		return
	}
	if content := m.wrappedContentString(); content == "" {
		m.styledLines = nil
	} else {
		m.styledLines = strings.Split(content, "\n")
	}
	m.styledLinesDirty = false
}

// SelectionState tracks the in-progress or finalized text selection.
// StartX/EndX are terminal cell columns (not byte offsets) into the plain line.
type SelectionState struct {
	Active   bool
	Dragging bool // true while the mouse button is held during a drag
	StartX   int  // cell column in plain content (0-based)
	StartY   int  // line in plain content (0-based)
	EndX     int
	EndY     int
}

// hasSelection reports whether there is a non-empty finalized or in-progress selection.
func (m *Model) hasSelection() bool {
	return m.selection != nil && m.selection.Active && m.getSelectedText() != ""
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
		m.statusMsg = fmt.Sprintf("Selected %d chars — ctrl+shift+c to copy", utf8.RuneCountInString(text))
	}
}

// copySelection copies the current selection to the clipboard.
// Returns true if a selection was present (even if copy failed).
// On failure the selection is kept so the user can retry.
func (m *Model) copySelection() bool {
	text := m.getSelectedText()
	if text == "" {
		return false
	}
	if err := copyToClipboard(text); err == nil {
		m.statusMsg = fmt.Sprintf("✓ Copied %d chars to clipboard", utf8.RuneCountInString(text))
		m.clearSelection()
	} else {
		m.statusMsg = fmt.Sprintf("Copy failed: %v", err)
	}
	return true
}

// runeWidth returns the terminal cell width of r (at least 1 for printable).
func runeWidth(r rune) int {
	w := ansi.StringWidth(string(r))
	if w < 1 {
		return 1
	}
	return w
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
		next := col + rw
		if next > start && col < end {
			b.WriteRune(r)
		}
		col = next
		if col >= end {
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
		next := col + rw
		if next > startCell && col < endCell {
			if startRi < 0 {
				startRi = ri
			}
			endRi = ri + 1
		}
		col = next
		ri++
		if col >= endCell {
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
func (m *Model) mouseToContent(mouseX, mouseY int) (int, int) {
	m.ensureWrappedLines()
	// Account for viewport scroll position. While dragging, freeze to the
	// YOffset captured at press so motion coordinates stay stable.
	contentY := mouseY + m.viewport.YOffset
	if m.selection != nil && m.selection.Dragging && m.selectionYOff >= 0 {
		contentY = mouseY + m.selectionYOff
	}
	if contentY < 0 || contentY >= len(m.wrappedLines) {
		return -1, -1
	}

	// Account for left padding (ViewportStyle has PaddingLeft(1))
	leftPad := m.viewport.Style.GetPaddingLeft()
	contentX := mouseX - leftPad
	if contentX < 0 {
		contentX = 0
	}

	line := m.wrappedLines[contentY]
	if max := ansi.StringWidth(line); contentX > max {
		contentX = max
	}

	return contentX, contentY
}

// getSelectedText returns the plain text currently selected, or "" if
// nothing is selected. StartX/EndX are cell columns.
func (m *Model) getSelectedText() string {
	m.ensureWrappedLines()
	if m.selection == nil || len(m.wrappedLines) == 0 {
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
		if y >= len(m.wrappedLines) {
			break
		}
		line := m.wrappedLines[y]
		ls := 0
		le := ansi.StringWidth(line)
		if y == startY {
			ls = startX
		}
		if y == endY {
			le = endX
		}
		chunk := sliceByCells(line, ls, le)
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

// renderViewportWithSelection renders the viewport content with selection
// highlighting applied. It replicates viewport.View() but injects reverse-video
// ANSI codes on selected text to preserve all original styling while adding
// the selection highlight.
func (m *Model) renderViewportWithSelection() string {
	m.ensureWrappedLines()
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
	m.ensureStyledLines()
	styledLines := m.styledLines

	// Normalize selection range
	selSY, selEY := m.selection.StartY, m.selection.EndY
	selSX, selEX := m.selection.StartX, m.selection.EndX
	if selSY > selEY || (selSY == selEY && selSX > selEX) {
		selSY, selEY = selEY, selSY
		selSX, selEX = selEX, selSX
	}

	// Match viewport.visibleLines: truncate (don't re-wrap) any line that
	// still exceeds contentWidth. lipgloss MaxWidth would soft-wrap those
	// into extra rows and shift everything below — the selection jump bug.
	mustCut := false
	for i := 0; i < contentHeight; i++ {
		ci := yOff + i
		if ci < len(styledLines) && ansi.StringWidth(styledLines[ci]) > contentWidth {
			mustCut = true
			break
		}
	}

	var lines []string
	for i := 0; i < contentHeight; i++ {
		ci := yOff + i
		if ci < len(styledLines) && ci < len(m.wrappedLines) {
			line := styledLines[ci]
			if ci >= selSY && ci <= selEY {
				lineWidth := ansi.StringWidth(m.wrappedLines[ci])
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
					line = highlightPlainRange(line, m.wrappedLines[ci], hs, he)
				}
			}
			if mustCut && contentWidth > 0 {
				line = ansi.Cut(line, 0, contentWidth)
			}
			lines = append(lines, line)
		} else {
			lines = append(lines, "")
		}
	}

	contents := lipgloss.NewStyle().
		Width(contentWidth).
		Height(contentHeight).
		MaxHeight(contentHeight).
		MaxWidth(contentWidth).
		Render(strings.Join(lines, "\n"))
	return m.viewport.Style.
		UnsetWidth().UnsetHeight().
		Render(contents)
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
		// Skip ANSI escape sequences (\x1b[...m etc.)
		if styled[i] == '\x1b' && i+1 < len(styled) && styled[i+1] == '[' {
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

// copyToClipboard tries clipboard.WriteAll first, then falls back to OSC52
// (terminal escape sequence) which works in modern terminals without xclip/xsel.
func copyToClipboard(text string) error {
	if err := clipboard.WriteAll(text); err == nil {
		return nil
	}
	// Fall back to OSC52: write escape sequence to stderr (stdout is TUI).
	// Most modern terminals (Alacritty, Kitty, iTerm2, GNOME Terminal,
	// Windows Terminal, etc.) support OSC52 even over SSH.
	seq := osc52.New(text)
	_, err := fmt.Fprint(os.Stderr, seq)
	return err
}
