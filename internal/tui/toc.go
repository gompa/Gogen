package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// tocAnchor is one user prompt in the transcript: the first WRAPPED line
// where it starts (an index into the viewport's line slice) and the raw
// prompt text for the hover preview. The slice is rebuilt on full
// transcript rebuilds (setViewportContent) and extended incrementally by
// the append funnels — the web's appendTocDot/rebuildToc lifecycle.
type tocAnchor struct {
	line int
	text string
}

const (
	// tocPreviewMaxChars caps the hover preview (web TOC_PREVIEW_MAX_CHARS).
	tocPreviewMaxChars = 300
	// tocMinChatWidth: below this the rail and preview would cover too
	// much of a narrow chat column, so the rail stays hidden.
	tocMinChatWidth = 40
)

// isUserPromptLine reports whether a (styled) chat line is a user prompt —
// the "You:" label line. Notice lines ("Notice:") are user-role injected
// but not prompts (web parity: dots mark real user bubbles only).
func isUserPromptLine(line string) bool {
	return strings.HasPrefix(ansi.Strip(line), userLabel+" ")
}

// promptTextOf returns the raw prompt text of a "You:" chat line.
func promptTextOf(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(ansi.Strip(line), userLabel+" "))
}

// tocAppendAnchor records a prompt line just appended at the tail. The
// append funnels move the old last line's wrap into prefixLines BEFORE
// appending, so prefixLines holds the wrapped lines of everything before
// the new last line — its length is the new line's physical start.
func (m *Model) tocAppendAnchor(line string) {
	m.tocAnchors = append(m.tocAnchors, tocAnchor{line: len(m.prefixLines), text: promptTextOf(line)})
}

// tocRefreshLastPrompt keeps the last anchor's text in sync when the last
// chat line (a prompt) is mutated in place (appendToLastLine /
// replaceLastLine). A no-op in the common case — prompt lines are not
// rewritten after being appended.
func (m *Model) tocRefreshLastPrompt() {
	if len(m.chatLines) == 0 || len(m.tocAnchors) == 0 {
		return
	}
	last := m.chatLines[len(m.chatLines)-1]
	a := &m.tocAnchors[len(m.tocAnchors)-1]
	if isUserPromptLine(last) && a.line == len(m.prefixLines) {
		a.text = promptTextOf(last)
	}
}

// tocActiveAnchor is the index of the prompt currently in view (the web's
// updateTocActive rule): the last prompt at or above the 35%-from-top
// line; the last prompt while at the bottom. -1 when there are no prompts.
func (m *Model) tocActiveAnchor() int {
	n := len(m.tocAnchors)
	if n == 0 {
		return -1
	}
	if m.viewport.AtBottom() {
		return n - 1
	}
	probe := m.viewport.YOffset + m.viewport.Height*35/100
	active := 0
	for i, a := range m.tocAnchors {
		if a.line > probe {
			break
		}
		active = i
	}
	return active
}

// tocAnchorAt maps a viewport row to the anchor whose prompt starts there
// (-1 when the row carries no dot).
func (m *Model) tocAnchorAt(y int) int {
	line := m.viewport.YOffset + y
	for i, a := range m.tocAnchors {
		if a.line == line {
			return i
		}
	}
	return -1
}

// tocJumpNext scrolls to the next user prompt below the current view top
// (next=true) or the last prompt above it (next=false) — the keyboard
// access to the rail (the web is mouse-only). False when there is no such
// prompt, so the caller can fall through to normal key handling.
func (m *Model) tocJumpNext(next bool) bool {
	if len(m.tocAnchors) == 0 {
		return false
	}
	if next {
		for _, a := range m.tocAnchors {
			if a.line > m.viewport.YOffset+1 {
				m.viewport.SetYOffset(a.line)
				return true
			}
		}
		return false
	}
	target := -1
	for _, a := range m.tocAnchors {
		if a.line < m.viewport.YOffset {
			target = a.line
		} else {
			break
		}
	}
	if target < 0 {
		return false
	}
	m.viewport.SetYOffset(target)
	return true
}

// handleTocMouse processes pointer events for the prompt rail and returns
// true when the event was consumed (it must not start a text selection).
// Wheel events always fall through to the viewport (web parity: the rail's
// wheel chains to the chat below).
func (m *Model) handleTocMouse(ev mouseEvent) bool {
	if ev.kind == mouseWheelEvent || m.modal != ModalNone {
		return false
	}
	chatW := m.mainWidth()
	enabled := len(m.tocAnchors) > 0 && chatW >= tocMinChatWidth
	railCol := m.sidebarOffsetX() + chatW - 1
	// Trigger strip: the chat's rightmost four cells — the terminal
	// equivalent of the web's ~30px hover zone around the dot column.
	inZone := enabled && ev.x >= railCol-3 && ev.x <= railCol &&
		ev.y >= 0 && ev.y < m.viewport.Height
	switch ev.kind {
	case mouseMotion:
		if inZone {
			m.tocHover = true
			m.tocPreview = m.tocAnchorAt(ev.y)
			return true // the strip never drives selection
		}
		if m.tocHover || m.tocPreview >= 0 {
			m.tocHover = false
			m.tocPreview = -1
		}
		return false
	case mousePress:
		if !inZone || ev.button != tea.MouseLeft {
			return false
		}
		if i := m.tocAnchorAt(ev.y); i >= 0 {
			// Web parity: the prompt jumps to the top of the viewport.
			m.viewport.SetYOffset(m.tocAnchors[i].line)
			m.tocPreview = -1
		}
		return true // presses in the strip never start a selection
	}
	return false
}

// applyTocOverlay pastes the prompt rail — and the hover preview when a
// dot is under the pointer — into the rendered frame. Dots occupy the
// viewport's right padding column (the chat column's last cell), so
// transcript text is never covered by a dot. The frame is returned
// unchanged when the rail is not shown (pointer elsewhere, no prompts,
// narrow column, or a modal up).
func (m *Model) applyTocOverlay(frame string) string {
	if !m.tocHover || m.modal != ModalNone || len(m.tocAnchors) == 0 {
		return frame
	}
	chatW := m.mainWidth()
	if chatW < tocMinChatWidth {
		return frame
	}
	lines := strings.Split(frame, "\n")
	railCol := m.sidebarOffsetX() + chatW - 1
	active := m.tocActiveAnchor()
	for i, a := range m.tocAnchors {
		sy := a.line - m.viewport.YOffset
		if sy < 0 || sy >= m.viewport.Height || sy >= len(lines) {
			continue
		}
		glyph, style := "·", ansiDimOn
		if i == active {
			glyph, style = "●", ansiCyanOn
		}
		pasteCell(&lines, sy, railCol, style+glyph+ansiReset)
	}
	if m.tocPreview >= 0 && m.tocPreview < len(m.tocAnchors) {
		if sy := m.tocAnchors[m.tocPreview].line - m.viewport.YOffset; sy >= 0 && sy < m.viewport.Height {
			m.pasteTocTooltip(lines, railCol, sy)
		}
	}
	return strings.Join(lines, "\n")
}

// pasteTocTooltip renders the hover preview (the web's showTocTooltip): a
// small bordered box with "Prompt N" and the raw text, opened to the LEFT
// of the rail and clamped inside the chat column and the viewport rows.
func (m *Model) pasteTocTooltip(lines []string, railCol, dotY int) {
	a := m.tocAnchors[m.tocPreview]
	text := a.text
	if r := []rune(text); len(r) > tocPreviewMaxChars {
		text = string(r[:tocPreviewMaxChars]) + "…"
	}
	chatW := m.mainWidth()
	w := min(40, chatW-6)
	if w < 12 {
		return
	}
	body := strings.Split(lipgloss.Wrap(text, w-4, " "), "\n")
	if len(body) > 6 {
		body = body[:6]
		body[5] += "…"
	}
	box := make([]string, 0, len(body)+3)
	box = append(box, "╭"+strings.Repeat("─", w-2)+"╮")
	label := ansiHighlightOn + "Prompt " + strconv.Itoa(m.tocPreview+1) + ansiReset
	box = append(box, "│  "+label+strings.Repeat(" ", max(0, w-4-ansi.StringWidth(label)))+ansiDimOn+"│"+ansiReset)
	for _, b := range body {
		box = append(box, "│  "+ansiDimOn+b+strings.Repeat(" ", max(0, w-4-ansi.StringWidth(b)))+"│"+ansiReset)
	}
	box = append(box, "╰"+strings.Repeat("─", w-2)+"╯")

	x := railCol - w - 1
	if x < m.sidebarOffsetX() {
		x = m.sidebarOffsetX()
	}
	y := dotY - len(box)/2
	if y < 0 {
		y = 0
	}
	if maxY := m.viewport.Height - len(box); y > maxY {
		y = max(0, maxY)
	}
	for i, row := range box {
		pasteCell(&lines, y+i, x, row)
	}
}

// pasteCell replaces the frame row's cells starting at column x with
// segment (ANSI-aware: surrounding cells survive and escape sequences are
// terminated at the cut points). Out-of-range rows are ignored; a segment
// running past the row's end is clipped.
func pasteCell(lines *[]string, y, x int, segment string) {
	if y < 0 || y >= len(*lines) {
		return
	}
	row := (*lines)[y]
	w := ansi.StringWidth(row)
	if x >= w {
		return
	}
	if sw := ansi.StringWidth(segment); x+sw > w {
		segment = ansi.Cut(segment, 0, w-x)
	}
	(*lines)[y] = ansi.Cut(row, 0, x) + segment + ansi.Cut(row, x+ansi.StringWidth(segment), w)
}
