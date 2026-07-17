package tui

import (
	"fmt"
	"os"
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

// SelectionState tracks the in-progress text selection.
type SelectionState struct {
	Active bool
	StartX int // column in plain content (0-based)
	StartY int // line in plain content (0-based)
	EndX   int
	EndY   int
}

// handleMouseSelection processes mouse events for text selection.
// Returns true if the event was consumed (selection handled), false if
// it should be passed through to the viewport for wheel scrolling.
func (m *Model) handleMouseSelection(msg tea.MouseMsg) bool {
	// Wheel events go to the viewport only when there's no active selection.
	// During selection, we block scrolling to keep coordinates stable.
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown ||
		msg.Button == tea.MouseButtonWheelLeft || msg.Button == tea.MouseButtonWheelRight {
		if m.selection != nil && m.selection.Active {
			return true // consume wheel during selection
		}
		return false
	}

	vpHeight := m.viewport.Height

	// Active selection: motion updates endpoint, release finalizes, anything
	// else that isn't a new left-press cancels.
	if m.selection != nil && m.selection.Active {
		switch {
		case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionMotion:
			x, y := m.mouseToContent(msg.X, msg.Y)
			if x >= 0 && y >= 0 {
				m.selection.EndX = x
				m.selection.EndY = y
			}
			return true
		case msg.Action == tea.MouseActionRelease:
			text := m.getSelectedText()
			m.selection = nil
			if text != "" {
				if err := copyToClipboard(text); err == nil {
					m.statusMsg = fmt.Sprintf("✓ Copied %d chars to clipboard", len(text))
				} else {
					m.statusMsg = fmt.Sprintf("Selected %d chars (copy failed: %v)", len(text), err)
				}
			} else {
				m.statusMsg = "(empty selection)"
			}
			return true
		default:
			// Any other mouse event (right click, new left press, etc.) cancels
			m.clearSelection()
			return false
		}
	}

	// No active selection: look for left-press inside viewport to start one
	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress &&
		msg.Y >= 0 && msg.Y < vpHeight {
		x, y := m.mouseToContent(msg.X, msg.Y)
		m.statusMsg = ""
		if x >= 0 && y >= 0 {
			m.selectionYOff = m.viewport.YOffset
			m.selection = &SelectionState{
				Active: true,
				StartX: x,
				StartY: y,
				EndX:   x,
				EndY:   y,
			}
		}
		return true
	}

	return false
}

// mouseToContent converts terminal-relative mouse coordinates to
// content coordinates (line and column in the plain wrapped content).
func (m *Model) mouseToContent(mouseX, mouseY int) (int, int) {
	m.ensureWrappedLines()
	// Account for viewport scroll position
	contentY := mouseY + m.viewport.YOffset
	if m.selection != nil && m.selection.Active && m.selectionYOff >= 0 {
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
	if contentX > len(line) {
		contentX = len(line)
	}

	return contentX, contentY
}

// getSelectedText returns the plain text currently selected, or "" if
// nothing is selected.
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
		le := len(line)
		if y == startY {
			ls = startX
		}
		if y == endY {
			le = endX
		}
		if ls > len(line) {
			ls = len(line)
		}
		if le > len(line) {
			le = len(line)
		}
		if le > ls {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line[ls:le])
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

	// Use selection start YOffset if available, otherwise current viewport offset.
	yOff := m.viewport.YOffset
	if m.selectionYOff >= 0 {
		yOff = m.selectionYOff
	}
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
				hs := 0
				he := len(m.wrappedLines[ci])
				if ci == selSY {
					hs = selSX
				}
				if ci == selEY {
					he = selEX
				}
				if hs > len(m.wrappedLines[ci]) {
					hs = len(m.wrappedLines[ci])
				}
				if he > len(m.wrappedLines[ci]) {
					he = len(m.wrappedLines[ci])
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
// into a styled string at a plain-text character range. It preserves all
// existing ANSI styling while adding the selection highlight on top.
func highlightPlainRange(styled, plain string, start, end int) string {
	if start >= end || start >= len(plain) {
		return styled
	}
	if end > len(plain) {
		end = len(plain)
	}

	styledStart := mapPlainToStyled(start, styled)
	styledEnd := mapPlainToStyled(end, styled)

	if styledStart >= styledEnd {
		return styled
	}

	return styled[:styledStart] + "\x1b[7m" + styled[styledStart:styledEnd] + "\x1b[27m" + styled[styledEnd:]
}

// mapPlainToStyled maps a plain-text character position to the corresponding
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
