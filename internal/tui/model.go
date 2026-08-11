package tui

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wrap"
)

// FocusTarget indicates which component has keyboard focus.
type FocusTarget int

const (
	FocusInput FocusTarget = iota
	FocusViewport
)

// ModalKind identifies the active modal overlay.
type ModalKind int

const (
	ModalNone ModalKind = iota
	ModalApproval
	ModalSessions
	ModalModels
	ModalHelp
	ModalCompletion
)

// Model is the top-level Bubble Tea model for the TUI.
type Model struct {
	// Core references
	agent *agent.Agent
	cfg   *config.Config

	// Runtime
	ctx     context.Context
	program *tea.Program

	// Components
	viewport Viewport
	textarea textarea.Model

	// State
	focus        FocusTarget
	modal        ModalKind
	streamCancel context.CancelFunc // cancels the current streaming LLM call
	streaming    bool
	verbose      bool
	width        int
	height       int
	quitting     bool

	// Wait indicator (input area) while a turn is in flight
	spinner       spinner.Model
	progressPhase progressPhase
	progressLabel string

	// Chat content buffer (lines to render)
	chatLines        []string
	wrappedPrefix    string   // pre-wrapped content of chatLines[:len-1], ends with "\n" if non-empty
	wrappedContent   string   // pre-computed wrapped content for viewport
	maxWrappedWidth  int      // cached max line width (incremental, avoids O(N) scan)
	styledLines      []string // wrappedContent split by "\n" (cached for selection render)
	styledLinesDirty bool     // true when styledLines needs recomputation
	// Incremental wrapping state: prefixLines holds the viewport lines for
	// chatLines[:len-1] so streaming updates splice only the last line's wrap
	// instead of re-splitting the whole conversation. wrappedContentDirty
	// defers the wrappedPrefix+lastWrapped concatenation until a consumer
	// (selection rendering) actually needs the full string.
	prefixLines         []string
	lastWrapped         string
	wrappedContentDirty bool

	// Streaming accumulation
	streamAssistantBuf  strings.Builder
	streamAssistantLine int // chatLines index for the live assistant line (-1 if none)
	streamThinkingBuf   strings.Builder
	streamThinkingOpen  bool
	streamThinkingLine  int               // chatLines index for the open thinking line (-1 if none)
	streamToolCallNames map[int]string    // index -> name
	streamToolCallArgs  map[int]string    // index -> accumulated args deltas
	streamToolCallIDs   map[int]string    // index -> call ID (for correlating results)
	streamToolCallLines map[int]int       // index -> chatLines index where the tool call line was added
	toolCallDiffs       map[string]string // call ID -> diff text (for patch_file/show_diff)
	activeToolName      string            // tool being prepared or executed; names the progress indicator

	streamToolDiffCount map[int]int     // index -> number of diff lines already rendered progressively
	streamToolDiffStart map[int]int     // index -> chatLines index where the first diff line is (after top border)
	toolDiffShown       map[string]bool // call ID -> true if diff was shown progressively (skip in result)
	// Input history
	inputHistory []string
	historyIdx   int
	historyDraft string

	// Context stats
	contextStats agent.TurnContext
	contextLine  string
	// contextStreamBaseUsed/contextStreamEstAdded support a live, approximate
	// context-indicator update while a turn is streaming. Reading a.Messages
	// during streaming (via refreshContextStats) races with the streaming
	// goroutine mutating it, so instead we snapshot the last authoritative
	// Used count at stream start and add a cheap local estimate for tokens
	// arriving via already-safe (Update-thread) stream messages. The exact
	// count is restored via refreshContextStats() once streaming ends.
	contextStreamBaseUsed int
	contextStreamEstAdded int

	// Session
	sessionID string

	// Completion state
	completions    []string
	completionIdx  int
	completionLine string // the full line at time of tab press

	// Approval state
	approvalResult   chan bool
	approvalUI       *approvalUIState
	approvalInFlight bool

	// Modal data
	sessionList   []agent.SessionInfo
	modelList     []llm.ModelInfo
	sessionCursor int
	modelCursor   int

	// Keymap
	keys KeyMap

	// Screen dimensions tracking
	ready bool

	// Text selection (mouse drag-to-select in viewport)
	selectionYOff     int // viewport YOffset at selection start (for stable coordinates)
	selection         *SelectionState
	wrappedLines      []string // ANSI-stripped wrapped content lines (coordinate mapping)
	wrappedLinesDirty bool
	// Render caches — recomputed only when inputs change.
	dividerCache       string
	dividerCacheWidth  int
	dividerCacheFocus  FocusTarget
	dividerCacheStream bool
	statusMsg          string // transient message (e.g. "Copied N chars")
}

// NewModel creates a new TUI model.
func NewModel(a *agent.Agent, cfg *config.Config) Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message or command..."
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.SetHeight(3)
	ta.MaxHeight = 8
	ta.CharLimit = 0 // no limit
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = InputPromptStyle
	ta.BlurredStyle.Prompt = InputPromptStyle
	ta.KeyMap.InsertNewline.SetEnabled(false) // we handle Enter ourselves

	// Remap textarea keybindings to match our CLI conventions
	ta.KeyMap.WordBackward = key.NewBinding(key.WithKeys("ctrl+left"))
	ta.KeyMap.WordForward = key.NewBinding(key.WithKeys("ctrl+right"))
	ta.KeyMap.DeleteWordBackward = key.NewBinding(key.WithKeys("ctrl+w"))
	ta.KeyMap.DeleteAfterCursor = key.NewBinding(key.WithKeys("ctrl+k"))
	ta.KeyMap.DeleteBeforeCursor = key.NewBinding(key.WithKeys("ctrl+u"))
	ta.KeyMap.LineStart = key.NewBinding(key.WithKeys("ctrl+a", "home"))
	ta.KeyMap.LineEnd = key.NewBinding(key.WithKeys("ctrl+e", "end"))
	ta.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("ctrl+d", "delete"))
	ta.KeyMap.DeleteCharacterBackward = key.NewBinding(key.WithKeys("backspace"))

	vp := NewViewport(80, 20)
	// Scroll 1 line per wheel event (default is 3; lower for smoother
	// scrolling when the mouse/terminal sends multiple events per notch).
	vp.MouseWheelDelta = 1
	vp.Style = ViewportStyle

	verbose := cfg != nil && cfg.CLIVerbose

	modelLine := ""
	if a != nil {
		modelLine = a.CurrentModel()
	}

	m := Model{
		agent:               a,
		cfg:                 cfg,
		viewport:            vp,
		textarea:            ta,
		focus:               FocusInput,
		modal:               ModalNone,
		streaming:           false,
		verbose:             verbose,
		spinner:             newProgressSpinner(),
		progressPhase:       progressHidden,
		chatLines:           make([]string, 0, 64), // pre-allocate to reduce GC during streaming
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
		streamToolCallNames: make(map[int]string),
		streamToolCallArgs:  make(map[int]string),
		streamToolCallIDs:   make(map[int]string),
		streamToolCallLines: make(map[int]int),
		toolCallDiffs:       make(map[string]string),
		streamToolDiffCount: make(map[int]int),
		streamToolDiffStart: make(map[int]int),
		toolDiffShown:       make(map[string]bool),
		keys:                DefaultKeyMap,
		sessionID:           "",
		approvalResult:      make(chan bool, 1),
		selectionYOff:       -1,
		selection:           nil,
		wrappedLines:        nil,
		statusMsg:           "",
	}

	if a != nil {
		m.sessionID = a.SessionID
		// Build initial history
		m.chatLines = renderMessages(a.Messages, a.WorkingDir, modelLine, a.Mode.String())
		m.setViewportContent()
		m.viewport.GotoBottom()
		if stats := a.ContextStats(context.Background()); stats.Snapshot.Limit > 0 || stats.Snapshot.Used > 0 {
			m.contextStats = stats
			m.contextLine = agent.FormatContextBrief(stats)
		}
	}

	return m
}

func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.ready = true

	// Layout: status bar (1 line) at bottom, textarea above it, viewport fills rest
	statusBarHeight := 1
	textareaHeight := m.textarea.Height()
	if textareaHeight > 8 {
		textareaHeight = 8
	}
	if textareaHeight < 1 {
		textareaHeight = 1
	}

	vpHeight := height - statusBarHeight - textareaHeight - 1 // -1 for textarea border
	if vpHeight < 3 {
		vpHeight = 3
	}

	m.viewport.Width = width - 2 // padding
	m.viewport.Height = vpHeight
	m.textarea.SetWidth(width - 2)
	m.textarea.SetHeight(textareaHeight)

	m.setViewportContent()
	m.viewport.GotoBottom()
}

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

func (m *Model) Init() tea.Cmd {
	return m.textarea.Focus()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Enhanced keyboard: ctrl+shift+c may arrive as an undecoded CSI sequence
	// (kitty / xterm modifyOtherKeys) rather than a KeyMsg.
	if _, ok := msg.(tea.KeyMsg); !ok && isCtrlShiftC(msg) {
		if m.statusMsg != "" {
			m.statusMsg = ""
		}
		m.copySelection()
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil

	case spinner.TickMsg:
		if !m.progressAnimating() {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		// Clear transient status message on any key press
		if m.statusMsg != "" {
			m.statusMsg = ""
		}
		if key.Matches(msg, m.keys.CopySelection) {
			m.copySelection()
			return m, nil
		}
		// Global hotkeys that work regardless of focus/modal
		switch {
		case key.Matches(msg, m.keys.ForceQuit):
			if m.streamCancel != nil {
				m.streamCancel()
			}
			m.dismissApproval(false)
			m.flushAndQuit()
			return m, tea.Quit
		}

		// If a modal is active, handle modal keys
		if m.modal != ModalNone {
			return m.handleModalKey(msg)
		}

		// Dispatch based on focus
		if m.focus == FocusViewport {
			return m.handleViewportKey(msg)
		}
		return m.handleInputKey(msg)

	// Streaming messages
	case streamStartMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamStart()
		return m, m.setProgress(progressThinking, "thinking")

	case streamRoundStartMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamRoundStart()
		return m, m.setProgress(progressThinking, "thinking")

	case streamTokenMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToken(msg.token)
		return m, m.setProgress(progressActive, "")

	case streamThinkingMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamThinking(msg.token)
		return m, m.setProgress(progressActive, "")

	case streamToolCallMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolCall(msg.index, msg.id, msg.name)
		return m, m.setProgress(progressActive, "")

	case streamToolCallArgsMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolArgs(msg.index, msg.id, msg.delta)
		return m, m.setProgress(progressActive, "")

	case streamToolCallFinalMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolCallFinal(msg.index, msg.tc)
		return m, nil

	case streamToolExecuteMsg:
		if !m.streaming {
			return m, nil
		}
		label := "running tool"
		if msg.name != "" {
			label = "running " + msg.name
		}
		return m, m.setProgress(progressTool, label)

	case streamToolResultMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolResult(msg.id, msg.name, msg.result, msg.success)
		return m, m.setProgress(progressThinking, "thinking")

	case streamRoundEndMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamRoundEnd()
		return m, m.setProgress(progressThinking, "thinking")

	case streamEndMsg:
		// Always process – resets streaming state
		m.handleStreamEnd()
		return m, m.refocusInput()

	case streamErrorMsg:
		m.handleStreamError(msg.err)
		return m, m.refocusInput()

	case contextStatsMsg:
		// Optional async refresh (session commands); stream end updates sync.
		m.contextStats = msg.stats
		m.contextLine = agent.FormatContextBrief(msg.stats)
		return m, nil

	// Approval request (show modal)
	case approvalRequestMsg:
		m.approvalUI = &approvalUIState{
			paths:  msg.req.Paths,
			reason: msg.req.Reason,
			cursor: 1, // default to Yes
		}
		m.modal = ModalApproval
		return m, nil

	// Pass mouse events to the viewport for wheel scrolling
	case tea.MouseMsg:
		// Check for text selection first; wheel events fall through to viewport
		if m.handleMouseSelection(msg) {
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)
	}

	// Update textarea for cursor blink and normal input
	if m.focus == FocusInput && m.modal == ModalNone && !m.streaming {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
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
