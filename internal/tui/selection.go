package tui

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// extractLeadingSGR returns the leading ANSI SGR sequence(s) from s.
// Only call when s starts with "\x1b["; returns "" otherwise. The
// return is safe to use as a prefix for continuation lines after
// word-wrapping.
func extractLeadingSGR(s string) string {
	if !strings.HasPrefix(s, "\x1b[") {
		return ""
	}
	// Scan consecutive escape sequences at the head of s. Lipgloss
	// emits them as \x1b[##m \x1b[##m ... so we consume every one
	// that follows directly.
	end := 0
	for end < len(s) {
		if end+1 >= len(s) || s[end] != '\x1b' || s[end+1] != '[' {
			break
		}
		j := end + 2 // past ESC[
		for j < len(s) && s[j] != '\x1b' {
			c := s[j]
			// Terminator letters for SGR: m (lower/upper).
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				j++
				break
			}
			// Digits, semicolons, colons (CSI parameter bytes).
			if (c < '0' || c > '9') && c != ';' && c != ':' {
				// Not a valid CSI byte — stop consuming this
				// sequence and let the outer loop exit.
				break
			}
			j++
		}
		end = j
	}
	return s[:end]
}

// extractTrailingSGR returns the concatenated ANSI SGR sequences that are
// still active at the end of s — i.e. every SGR sequence that appears after
// the last \x1b[0m reset.  Returns "" when no SGR is active at the end.
//
// This is used to correctly propagate the *current* style at a wrap point,
// which may differ from the leading style when a line contains multiple
// independently-styled segments (e.g. a tool-call prefix followed by
// dimmed arguments).
func extractTrailingSGR(s string) string {
	// Find the tail after the last reset.
	lastReset := strings.LastIndex(s, "\x1b[0m")
	start := 0
	if lastReset >= 0 {
		start = lastReset + 4
	}
	if start >= len(s) {
		return ""
	}
	tail := s[start:]

	// Scan the tail for SGR sequences — collect every one we find.
	// Non-SGR text between them is allowed (e.g. " name \x1b[37margs").
	idx := strings.Index(tail, "\x1b[")
	if idx < 0 {
		return ""
	}
	tail = tail[idx:] // skip any plain text before the first SGR in the tail

	// Now collect consecutive SGR sequences from the head of what remains.
	end := 0
	for end < len(tail) {
		if end+1 >= len(tail) || tail[end] != '\x1b' || tail[end+1] != '[' {
			break
		}
		j := end + 2
		for j < len(tail) && tail[j] != '\x1b' {
			c := tail[j]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				j++
				break
			}
			if (c < '0' || c > '9') && c != ';' && c != ':' {
				break
			}
			j++
		}
		end = j
	}
	return tail[:end]
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
	if m.wrappedContent == "" {
		m.wrappedLines = nil
	} else {
		m.wrappedLines = strings.Split(stripANSI(m.wrappedContent), "\n")
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
	if m.wrappedContent == "" {
		m.styledLines = nil
	} else {
		m.styledLines = strings.Split(m.wrappedContent, "\n")
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

// handleMouseSelection processes mouse events for text selection.
// Returns true if the event was consumed (selection handled), false if
// it should be passed through to the viewport for wheel scrolling.
func (m *Model) handleMouseSelection(msg tea.MouseMsg) bool {
	// Block wheel only while dragging so content coordinates stay stable.
	// After release, scrolling is allowed and the highlight tracks content.
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown ||
		msg.Button == tea.MouseButtonWheelLeft || msg.Button == tea.MouseButtonWheelRight {
		if m.selection != nil && m.selection.Dragging {
			return true
		}
		return false
	}

	vpHeight := m.viewport.Height

	if m.selection != nil && m.selection.Active {
		switch {
		case m.selection.Dragging && msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionMotion:
			x, y := m.mouseToContent(msg.X, msg.Y)
			if x >= 0 && y >= 0 {
				m.selection.EndX = x
				m.selection.EndY = y
			}
			return true
		case m.selection.Dragging && msg.Action == tea.MouseActionRelease:
			m.selection.Dragging = false
			m.selectionYOff = -1 // unlock scroll; coords are content-absolute
			text := m.getSelectedText()
			if text == "" {
				m.clearSelection()
				m.statusMsg = ""
			} else {
				m.statusMsg = fmt.Sprintf("Selected %d chars — ctrl+shift+c to copy", utf8.RuneCountInString(text))
			}
			return true
		case msg.Button == tea.MouseButtonRight &&
			(msg.Action == tea.MouseActionPress || msg.Action == tea.MouseActionRelease):
			m.clearSelection()
			m.statusMsg = ""
			return true
		case !m.selection.Dragging && msg.Button == tea.MouseButtonLeft &&
			msg.Action == tea.MouseActionPress && msg.Y >= 0 && msg.Y < vpHeight:
			// Replace the existing selection; start logic below the if handles it.
			m.clearSelection()
		case m.selection.Dragging:
			// Ignore other events while dragging (e.g. ButtonNone motion that
			// some terminals emit). Do NOT clear — that was wiping the
			// selection before the user could copy.
			return true
		default:
			return false
		}
	}

	// No active selection (or just cleared): left-press inside viewport starts one.
	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress &&
		msg.Y >= 0 && msg.Y < vpHeight {
		x, y := m.mouseToContent(msg.X, msg.Y)
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

// isCtrlShiftC reports whether msg is Ctrl+Shift+C.
// Bubble Tea v1 only decodes that combo for special keys; modern terminals
// often send kitty/xterm CSI sequences that arrive as an opaque []byte-based msg.
func isCtrlShiftC(msg tea.Msg) bool {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "ctrl+shift+c", "ctrl+shift+C":
			return true
		}
		return false
	}
	b := byteSliceMsg(msg)
	if len(b) == 0 {
		return false
	}
	// kitty keyboard protocol: CSI 99 ; 6 u  (99='c', 6=ctrl+shift)
	// xterm modifyOtherKeys:   CSI 27 ; 6 ; 99 ~
	return bytes.Equal(b, []byte("\x1b[99;6u")) ||
		bytes.Equal(b, []byte("\x1b[99;6U")) ||
		bytes.Equal(b, []byte("\x1b[27;6;99~"))
}

// byteSliceMsg extracts a []byte payload from msg.
// Bubble Tea's unknownCSISequenceMsg is an unexported defined []byte type,
// so a plain []byte assert is not enough — reflect handles that case.
func byteSliceMsg(msg tea.Msg) []byte {
	if b, ok := msg.([]byte); ok {
		return b
	}
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		return append([]byte(nil), v.Bytes()...)
	}
	return nil
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
