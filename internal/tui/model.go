package tui

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wrap"
)

// wrapWidth returns the available width for word-wrapping inside the viewport.
func (m *Model) wrapWidth() int {
	w := m.viewport.Width
	if w < 10 {
		w = 10
	}
	w -= m.viewport.Style.GetHorizontalFrameSize()
	if w < 10 {
		w = 10
	}
	return w
}

// wrapLine wraps a single chat line ready for display.  It handles SGR
// propagation so that ANSI styles are re‑emitted on every continuation line.
//
// wrap.String handles both word-wrapping and hard-wrapping of overlong tokens
// (URLs, paths, etc.) in a single pass, avoiding the cost of double-wrapping
// on every streaming update (~32ms batches).
func (m *Model) wrapLine(line string) []string {
	w := m.wrapWidth()
	wrapped := wrap.String(line, w)
	parts := strings.Split(wrapped, "\n")
	// Strip trailing empty elements caused by a trailing newline.
	// Without this, a trailing \n creates a blank visual line that
	// flickers during streaming or persists in the final output.
	for len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 1 {
		// Propagate the style that is active at each wrap point, tracked
		// as a running state across every part rather than computed once
		// from parts[0]. A styled segment can begin partway through a
		// later continuation line (e.g. a plain tool-call prefix followed
		// by dimmed args that themselves span several more wrapped
		// lines), so the active style must be re-derived after each part
		// — otherwise everything past the part where the style *first*
		// turns up loses it entirely once earlier lines scroll out of
		// view (and, when a line hands off between two different
		// styles, a stale one can leak into text that should carry the
		// next style instead).
		active := extractTrailingSGR(parts[0])
		if active == "" && !strings.Contains(parts[0], "\x1b[0m") {
			// No reset in parts[0]: the leading style is still open.
			active = extractLeadingSGR(line)
		}
		for i := 1; i < len(parts); i++ {
			if parts[i] == "" {
				continue // skip SGR on empty continuation lines
			}
			orig := parts[i]
			if active != "" {
				parts[i] = active + orig + "\x1b[0m"
			}
			// Re-derive the active style for the *next* part from this
			// part's own (pre-propagation) content.
			if trailing := extractTrailingSGR(orig); trailing != "" {
				active = trailing
			} else if strings.Contains(orig, "\x1b[0m") {
				// This part closes with its own reset and nothing
				// styled after it: the style that was active has ended.
				active = ""
			}
			// else: orig has no SGR of its own at all — whatever was
			// active carries over unchanged into the next part.
		}
	}
	return parts
}

// buildFromPrefix rebuilds the viewport from wrappedPrefix + the last chat
// line. All incremental updaters call this instead of the full re-wrap path.
//
// During streaming, this is the hot path called on every token batch (~32 ms).
// Only the last chat line changes, so we re-wrap just that line and splice it
// onto the cached prefixLines (viewport lines for chatLines[:len-1]) instead
// of re-splitting the entire wrapped content into lines on every flush — the
// old SetContentMax path was O(conversation) per token batch. The max line
// width is likewise maintained incrementally (only the new last line is
// measured), skipping the O(N) ansi.StringWidth scan of the full conversation
// that the stock bubbles viewport performs. The full wrappedContent string is
// materialized lazily via wrappedContentString() for consumers that need it
// (selection rendering); streaming itself only needs the split lines.
func (m *Model) buildFromPrefix() {
	if len(m.chatLines) == 0 {
		m.wrappedContent = ""
		m.wrappedLines = nil
		m.wrappedLinesDirty = false
		m.maxWrappedWidth = 0
		m.lastWrapped = ""
		m.wrappedContentDirty = false
		m.prefixLines = nil
		m.clearSelection()
		m.viewport.SetContentMax("", 0)
		// styledLines is computed lazily in ensureStyledLines().
		m.styledLinesDirty = true
		return
	}
	lastParts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
	lastWrapped := strings.Join(lastParts, "\n")
	m.lastWrapped = lastWrapped
	m.wrappedContentDirty = true
	m.wrappedLinesDirty = true
	// Incremental max width: only scan the newly wrapped last line.
	for _, p := range lastParts {
		if w := ansi.StringWidth(p); w > m.maxWrappedWidth {
			m.maxWrappedWidth = w
		}
	}
	// Splice the freshly wrapped last line onto the cached prefix lines.
	lastLines := strings.Split(lastWrapped, "\n")
	vp := m.viewport.Lines()
	vp = vp[:0]
	vp = append(vp, m.prefixLines...)
	vp = append(vp, lastLines...)
	m.viewport.SetContentLines(vp, m.maxWrappedWidth)
	m.clearSelection()
	// styledLines is computed lazily in ensureStyledLines().
	m.styledLinesDirty = true
}

// wrappedContentString returns the full wrapped chat content, materializing
// the lazily-maintained wrappedPrefix+lastWrapped concatenation on first
// access. Selection rendering and tests are the only consumers; streaming
// updates avoid building (and re-splitting) the full string on every token
// batch. The cached value is byte-identical to what buildFromPrefix used to
// compute eagerly.
func (m *Model) wrappedContentString() string {
	if m.wrappedContentDirty {
		m.wrappedContent = m.wrappedPrefix + m.lastWrapped
		m.wrappedContentDirty = false
	}
	return m.wrappedContent
}

// setViewportContent performs a full re-wrap of all chatLines and rebuilds
// the incremental prefix.  Use this after window‑resize, session restore,
// mode changes, or other events that touch the whole buffer.
func (m *Model) setViewportContent() {
	if m.width <= 2 {
		return
	}
	var wrappedParts []string
	var lastParts []string
	for _, line := range m.chatLines {
		lastParts = m.wrapLine(line)
		wrappedParts = append(wrappedParts, lastParts...)
	}
	m.wrappedContent = strings.Join(wrappedParts, "\n")
	m.wrappedLinesDirty = true // lazily compute on next selection access
	m.clearSelection()
	// Full re-scan of all lines — acceptable because this is called rarely
	// (resize, session restore, mode changes).
	m.maxWrappedWidth = 0
	for _, p := range wrappedParts {
		if w := ansi.StringWidth(p); w > m.maxWrappedWidth {
			m.maxWrappedWidth = w
		}
	}
	m.viewport.SetContentMax(m.wrappedContent, m.maxWrappedWidth)
	// styledLines is computed lazily in ensureStyledLines().
	m.styledLinesDirty = true

	// Rebuild the prefix pointing at all lines except the last.
	if len(m.chatLines) > 1 {
		var prefixParts []string
		for _, line := range m.chatLines[:len(m.chatLines)-1] {
			prefixParts = append(prefixParts, m.wrapLine(line)...)
		}
		m.wrappedPrefix = strings.Join(prefixParts, "\n") + "\n"
		m.prefixLines = prefixParts
	} else {
		m.wrappedPrefix = ""
		m.prefixLines = nil
	}
	// The full content was materialized above; keep the incremental state
	// consistent so the next buildFromPrefix splices onto the right prefix.
	m.lastWrapped = strings.Join(lastParts, "\n")
	m.wrappedContentDirty = false
}

func (m *Model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	if m.quitting {
		return ""
	}

	// Viewport content: use selection-aware render when selecting,
	// otherwise use the stock viewport render.
	var vpView string
	if m.selection != nil && m.selection.Active {
		vpView = m.renderViewportWithSelection()
	} else {
		vpView = m.viewport.View()
	}

	// Textarea
	var inputArea string
	if m.streaming {
		inputArea = m.renderProgressInput()
	} else {
		inputArea = m.textarea.View()
	}

	// Divider with focus indicator.  Cache the rendered string and only
	// rebuild when width, focus, or streaming state change.
	dividerDirty := m.dividerCacheWidth != m.width ||
		m.dividerCacheFocus != m.focus ||
		m.dividerCacheStream != m.streaming
	if dividerDirty {
		if m.focus == FocusViewport {
			indicator := " [SCROLL] Press i or Esc to return to input "
			line := strings.Repeat("─", m.width)
			keep := max(0, m.width-len(indicator))
			m.dividerCache = DividerStyle.Render(sliceByRuneCount(line, keep) + indicator)
		} else if m.streaming {
			m.dividerCache = DimStyle.Render(strings.Repeat("─", m.width))
		} else {
			m.dividerCache = DividerStyle.Render(strings.Repeat("─", m.width))
		}
		m.dividerCacheWidth = m.width
		m.dividerCacheFocus = m.focus
		m.dividerCacheStream = m.streaming
	}
	divider := m.dividerCache

	// Assemble
	main := lipgloss.JoinVertical(
		lipgloss.Left,
		vpView,
		divider,
		inputArea,
		m.renderStatusBar(),
	)

	// Modal overlay — renders on opaque background so nothing bleeds through
	if m.modal != ModalNone {
		return renderModalOverlay(main, m.renderModal(), m.width, m.height)
	}

	return main
}

// renderModalOverlay dims the main view and centers the modal on top.
func renderModalOverlay(main, modal string, width, height int) string {
	modalWidth := lipgloss.Width(modal)
	modalHeight := lipgloss.Height(modal)

	// Pad horizontally to center
	leftPad := max(0, (width-modalWidth)/2)

	// Pad vertically to center
	topPad := max(0, (height-modalHeight)/2)
	bottomPad := max(0, height-modalHeight-topPad)

	var b strings.Builder
	for i := 0; i < topPad; i++ {
		b.WriteString(strings.Repeat(" ", width) + "\n")
	}
	for _, line := range strings.Split(modal, "\n") {
		b.WriteString(strings.Repeat(" ", leftPad))
		b.WriteString(line)
		b.WriteString(strings.Repeat(" ", max(0, width-leftPad-lipgloss.Width(line))))
		b.WriteByte('\n')
	}
	for i := 0; i < bottomPad; i++ {
		b.WriteString(strings.Repeat(" ", width) + "\n")
	}

	return ModalOverlayBackground.Render(strings.TrimRight(b.String(), "\n"))
}

// refocusInput restarts the textarea cursor blink after streaming (blink ticks
// are ignored while streaming==true, so the blink loop must be restarted).
func (m *Model) refocusInput() tea.Cmd {
	if m.focus != FocusInput || m.modal != ModalNone {
		return nil
	}
	return m.textarea.Focus()
}

// estimateTokenCount is a cheap, tokenizer-free approximation (~4 chars per
// token for English-like text) used only to keep the context indicator
// moving live during streaming. It is intentionally rough — the exact count
// is restored by refreshContextStats() as soon as streaming ends.
func estimateTokenCount(s string) int {
	if s == "" {
		return 0
	}
	n := (len(s) + 3) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// bumpContextEstimate updates the status-bar context indicator with an
// approximate running total while a turn is streaming. It never reads
// a.Messages (which would race with the streaming goroutine mutating it) —
// it only combines the baseline captured in handleStreamStart with a local
// character-based estimate of text that has already safely arrived on the
// Update thread via stream messages.
func (m *Model) bumpContextEstimate(delta string) {
	if delta == "" {
		return
	}
	if m.contextStreamBaseUsed <= 0 && m.contextStats.Snapshot.Limit <= 0 {
		// No baseline yet (e.g. first turn before any refresh) — nothing
		// meaningful to show until refreshContextStats() runs.
		return
	}
	m.contextStreamEstAdded += estimateTokenCount(delta)
	snap := m.contextStats.Snapshot
	snap.Used = m.contextStreamBaseUsed + m.contextStreamEstAdded
	if snap.Limit > 0 {
		snap.Percent = float64(snap.Used) / float64(snap.Limit)
	}
	display := m.contextStats
	display.Snapshot = snap
	if line := agent.FormatContextBrief(display); line != "" {
		m.contextLine = line + " (est.)"
	}
}

// refreshContextStats updates the status-bar context indicator immediately.
// Only call when StreamProcessInput is not running (no Messages race).
func (m *Model) refreshContextStats() {
	if m.agent == nil {
		m.contextStats = agent.TurnContext{}
		m.contextLine = ""
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	stats := m.agent.ContextStats(ctx)
	m.contextStats = stats
	m.contextLine = agent.FormatContextBrief(stats)
}

// flushAndQuit forces a final session write before the program exits.
// Without this, the 5 s debounce could drop the last few seconds of state.
func (m *Model) flushAndQuit() {
	m.quitting = true
	if m.agent != nil {
		m.agent.FlushSession()
	}
}

// checkPersistError surfaces any pending session-save error in the status
// bar. Call from any code path that may have triggered Agent.persistSession
// (slash commands, /rename tool, etc.) so silent save failures aren't lost.
func (m *Model) checkPersistError() {
	if m.agent == nil {
		return
	}
	if err := m.agent.ConsumePersistError(); err != nil {
		m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", err)
	}
}

func (m *Model) handleStreamError(err error) {
	wasStreaming := m.streaming
	m.streaming = false
	m.dismissApproval(false)
	m.clearProgress()
	m.resetStreamState(false)
	m.refreshContextStats()
	if m.agent != nil {
		if persistErr := m.agent.ConsumePersistError(); persistErr != nil {
			m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", persistErr)
		}
	}
	if err == nil {
		return
	}
	// UI cancel already printed "Cancelled." — don't duplicate context.Canceled.
	if !wasStreaming && (err == context.Canceled || strings.Contains(err.Error(), "context canceled")) {
		return
	}
	m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Error: %v", err)))
}

// sliceByRuneCount returns the prefix of s containing at most n runes.
// Uses rune-counting so it does not split multi-byte UTF-8 characters.
func sliceByRuneCount(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if n >= len(runes) {
		return s
	}
	return string(runes[:n])
}

func summarizeResult(result string, success bool) string {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		if success {
			return "(empty)"
		}
		return "(no output)"
	}
	lines := strings.Count(trimmed, "\n") + 1
	chars := len(trimmed)
	if !success {
		first := trimmed
		if idx := strings.Index(first, "\n"); idx >= 0 {
			first = first[:idx]
		}
		if len(first) > 120 {
			first = truncateRunes(first, 117) + "..."
		}
		return fmt.Sprintf("%s (%d chars)", first, chars)
	}
	if lines == 1 && chars <= 120 {
		return trimmed
	}
	return fmt.Sprintf("(%d lines, %d chars)", lines, chars)
}
