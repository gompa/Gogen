package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/session"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

// wrapLine wraps a single chat line ready for display. lipgloss.Wrap does
// word-wrapping plus hard-wrapping of overlong tokens (URLs, paths, etc.) in
// one pass, and re-applies any SGR style that is still open onto every
// continuation line while closing it before end-of-line — so styles survive
// wrapping without manual SGR bookkeeping here (the hand-rolled tracker this
// replaced was the source of the v2 label-bleed bug fixed in a3b358b).
func (m *Model) wrapLine(line string) []string {
	w := m.wrapWidth()
	wrapped := lipgloss.Wrap(line, w, "")
	parts := strings.Split(wrapped, "\n")
	// Strip trailing empty elements caused by a trailing newline.
	// Without this, a trailing \n creates a blank visual line that
	// flickers during streaming or persists in the final output.
	for len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
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

func (m *Model) View() tea.View {
	if !m.ready {
		return tea.NewView("Initializing...")
	}

	if m.quitting {
		return tea.NewView("")
	}

	main := m.renderMainColumn()

	// Sidebar panel (live sessions) — only when visible AND the main
	// column keeps its minimum width (mainWidth auto-hides otherwise).
	if m.sidebarOffsetX() > 0 {
		mainLines := strings.Count(main, "\n") + 1
		main = lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(mainLines), main)
	}

	v := tea.NewView(main)
	// Mouse reporting: cell motion covers wheel scrolls and drag-selection.
	v.MouseMode = tea.MouseModeCellMotion
	// Focus reporting: tea.FocusMsg / tea.BlurMsg drive the completion
	// bell (bellIfBlurred) — it rings only while the window is blurred.
	v.ReportFocus = true

	// Modal overlay — renders on opaque background so nothing bleeds through
	if m.modal != ModalNone {
		return tea.NewView(renderModalOverlay(m.renderModal(), m.width, m.height))
	}

	return v
}

// renderMainColumn assembles the chat column (viewport, divider, input,
// status bar) WITHOUT the sidebar. Kept separate so the sidebar can match
// its exact row height in View.
func (m *Model) renderMainColumn() string {
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
	// rebuild when width, focus, or streaming state change. Sized to the
	// MAIN COLUMN (not the terminal): with the sidebar visible the column
	// is narrower, and an over-wide divider would grow the combined frame
	// past the terminal.
	main := m.mainWidth()
	dividerDirty := m.dividerCacheWidth != main ||
		m.dividerCacheFocus != m.focus ||
		m.dividerCacheStream != m.streaming
	if dividerDirty {
		if m.focus == FocusViewport {
			indicator := " [SCROLL] Press i or Esc to return to input "
			line := strings.Repeat("─", main)
			keep := max(0, main-len(indicator))
			m.dividerCache = DividerStyle.Render(sliceByRuneCount(line, keep) + indicator)
		} else if m.streaming {
			m.dividerCache = DimStyle.Render(strings.Repeat("─", main))
		} else {
			m.dividerCache = DividerStyle.Render(strings.Repeat("─", main))
		}
		m.dividerCacheWidth = main
		m.dividerCacheFocus = m.focus
		m.dividerCacheStream = m.streaming
	}
	divider := m.dividerCache

	// Assemble
	return lipgloss.JoinVertical(
		lipgloss.Left,
		vpView,
		divider,
		inputArea,
		m.renderStatusBar(),
	)
}

// refreshSavedSessions snapshots the persisted-session index backing the
// sidebar's unified list (best-effort; errors leave the cache empty). The
// store's 1 s list cache makes this cheap to call on every turn end.
func (m *Model) refreshSavedSessions() {
	if m.agent == nil {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	result, _, err := m.agent.HandleSessionCommand(ctx, "sessions", session.NewID())
	if err != nil {
		return
	}
	m.savedCache = result.Sessions
}

// sliceRuneLen counts runes (sliceByRuneCount's measure companion).
func sliceRuneLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// renderModalOverlay covers the main view with an opaque background block
// and centers the modal on top of it. lipgloss.Place is a noöp when the
// modal exceeds the terminal in either dimension, matching the old manual
// clamping; ModalOverlayBackground paints every cell, padding included.
func renderModalOverlay(modal string, width, height int) string {
	return ModalOverlayBackground.Render(
		lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal),
	)
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
// ContextStats is safe to call concurrently with a running turn (it
// snapshots shared state under statsMu), but this unguarded variant is
// reserved for paths where a minimal snapshot is acceptable (stream end,
// error, session changes); mid-turn callers should use
// refreshContextStatsMidTurn, which never blanks the indicator.
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

// refreshContextStatsMidTurn refreshes the context indicator from the agent
// at a round boundary while a turn is streaming. ContextStats is safe to
// call concurrently with a running turn (it snapshots shared state under
// statsMu and tokenizes a clone), so this does not race the streaming
// goroutine. A minimal snapshot (e.g. the caller context was cancelled and
// ContextStats short-circuited) is not adopted, so the indicator never
// blanks out mid-turn.
func (m *Model) refreshContextStatsMidTurn() {
	if m.agent == nil {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	stats := m.agent.ContextStats(ctx)
	if stats.Snapshot.Used <= 0 && stats.Snapshot.Limit <= 0 {
		return
	}
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
	// Fan out to background live sessions: each owns its persisted state,
	// and the debounce window is per-agent — a session whose last turn
	// ended just before quit would otherwise lose its tail.
	if m.lives != nil {
		for _, s := range m.lives.sessions {
			if s.agent == nil || s.agent == m.agent {
				continue
			}
			s.agent.FlushSession()
		}
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

// bellIfBlurred rings the terminal bell while the window is NOT focused —
// the TUI equivalent of the web's desktop notifications (turn end, turn
// error, approval request). No bell while the user is looking at the
// terminal; terminals that don't report focus never blur, so they never
// bell.
func (m *Model) bellIfBlurred() {
	if !m.terminalBlurred {
		return
	}
	m.bellsRung++
	// BEL to the tty (the program's default output). A lone control
	// character interleaved with renderer frames is harmless — the
	// terminal emulates it, it never shifts the cursor. The program-nil
	// guard keeps literal-built test models quiet.
	if m.program != nil {
		fmt.Fprint(os.Stdout, "\a")
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
	m.bellIfBlurred() // web parity: turn-error notification (cancels stay quiet)
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
