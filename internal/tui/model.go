package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
)

// FocusTarget indicates which component has keyboard focus.
type FocusTarget int

const (
	FocusInput    FocusTarget = iota
	FocusViewport FocusTarget = iota
)

// ModalKind identifies the active modal overlay.
type ModalKind int

const (
	ModalNone       ModalKind = iota
	ModalApproval   ModalKind = iota
	ModalSessions   ModalKind = iota
	ModalModels     ModalKind = iota
	ModalHelp       ModalKind = iota
	ModalCompletion ModalKind = iota
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
	viewport viewport.Model
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
	chatLines      []string
	wrappedPrefix  string   // pre-wrapped content of chatLines[:len-1], ends with "\n" if non-empty
	wrappedContent string   // pre-computed wrapped content for viewport
	styledLines    []string // wrappedContent split by "\n" (cached for selection render)

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

	// Session
	sessionID string

	// Completion state
	completions    []string
	completionIdx  int
	completionLine string // the full line at time of tab press

	// Approval state
	approvalResult chan bool
	approvalUI     *approvalUIState

	// Modal data
	sessionList   []agent.SessionInfo
	modelList     []llm.ModelInfo
	sessionCursor int

	// Keymap
	keys KeyMap

	// Screen dimensions tracking
	ready bool

	// Text selection (mouse drag-to-select in viewport)
	selectionYOff     int // viewport YOffset at selection start (for stable coordinates)
	selection         *SelectionState
	wrappedLines      []string // ANSI-stripped wrapped content lines (coordinate mapping)
	wrappedLinesDirty bool
	statusMsg         string // transient message (e.g. "Copied N chars")
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

	vp := viewport.New(80, 20)
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
		chatLines:           make([]string, 0),
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
// wordwrap alone leaves overlong tokens (URLs, paths, etc.) wider than the
// limit. A follow-up hard wrap breaks those so every line fits wrapWidth.
// Without that, the normal viewport truncates with ansi.Cut while the
// selection renderer re-wraps via lipgloss MaxWidth — causing lines to jump
// when selecting text.
func (m *Model) wrapLine(line string) []string {
	w := m.wrapWidth()
	wrapped := wordwrap.String(line, w)
	wrapped = wrap.String(wrapped, w)
	parts := strings.Split(wrapped, "\n")
	// Strip trailing empty elements caused by a trailing newline.
	// Without this, a trailing \n creates a blank visual line that
	// flickers during streaming or persists in the final output.
	for len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 1 {
		sgr := extractLeadingSGR(line)
		if sgr != "" && !strings.Contains(parts[0], "\x1b[0m") {
			for i := 1; i < len(parts); i++ {
				if parts[i] == "" {
					continue // skip SGR on empty continuation lines
				}
				parts[i] = sgr + parts[i] + "\x1b[0m"
			}
		}
	}
	return parts
}

// buildFromPrefix computes wrappedContent from wrappedPrefix + the last chat
// line.  All incremental updaters call this instead of the full re-wrap path.
func (m *Model) buildFromPrefix() {
	if len(m.chatLines) == 0 {
		m.wrappedContent = ""
		m.wrappedLines = nil
		m.wrappedLinesDirty = false
	} else {
		lastWrapped := strings.Join(m.wrapLine(m.chatLines[len(m.chatLines)-1]), "\n")
		m.wrappedContent = m.wrappedPrefix + lastWrapped
		m.wrappedLinesDirty = true // lazily compute on next selection access
	}
	m.clearSelection()
	m.viewport.SetContent(m.wrappedContent)
	m.styledLines = strings.Split(m.wrappedContent, "\n")
}

// setViewportContent performs a full re-wrap of all chatLines and rebuilds
// the incremental prefix.  Use this after window‑resize, session restore,
// mode changes, or other events that touch the whole buffer.
func (m *Model) setViewportContent() {
	if m.width <= 2 {
		return
	}
	var wrappedParts []string
	for _, line := range m.chatLines {
		wrappedParts = append(wrappedParts, m.wrapLine(line)...)
	}
	m.wrappedContent = strings.Join(wrappedParts, "\n")
	m.wrappedLinesDirty = true // lazily compute on next selection access
	m.clearSelection()
	m.viewport.SetContent(m.wrappedContent)
	m.styledLines = strings.Split(m.wrappedContent, "\n")

	// Rebuild the prefix pointing at all lines except the last.
	if len(m.chatLines) > 1 {
		var prefixParts []string
		for _, line := range m.chatLines[:len(m.chatLines)-1] {
			prefixParts = append(prefixParts, m.wrapLine(line)...)
		}
		m.wrappedPrefix = strings.Join(prefixParts, "\n") + "\n"
	} else {
		m.wrappedPrefix = ""
	}
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
			m.quitting = true
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

	// Session/modal results
	case sessionRestoredMsg:
		m.sessionID = msg.sessionID
		m.chatLines = renderMessages(msg.messages, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())
		m.setViewportContent()
		m.viewport.GotoBottom()
		return m, nil

	case clearChatMsg:
		m.chatLines = nil
		m.setViewportContent()
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

func (m Model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	if m.quitting {
		return ""
	}

	// Build the main layout
	statusBar := m.renderStatusBar()

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

	// Divider with focus indicator
	var divider string
	if m.focus == FocusViewport {
		indicator := " [SCROLL] Press i or Esc to return to input "
		line := strings.Repeat("─", m.width)
		keep := max(0, m.width-len(indicator))
		divider = DividerStyle.Render(sliceByRuneCount(line, keep) + indicator)
	} else if m.streaming {
		divider = DimStyle.Render(strings.Repeat("─", m.width))
	} else {
		divider = DividerStyle.Render(strings.Repeat("─", m.width))
	}

	// Assemble
	main := lipgloss.JoinVertical(
		lipgloss.Left,
		vpView,
		divider,
		inputArea,
		statusBar,
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

// appendChatLine adds a line to the chat buffer and updates the viewport.
func (m *Model) appendChatLine(line string) {
	// Move the current last line's wrapping into the prefix so that only
	// the *new* last line needs re-wrapping on subsequent updates.
	if len(m.chatLines) > 0 {
		parts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
		if m.wrappedPrefix == "" {
			m.wrappedPrefix = strings.Join(parts, "\n")
		} else {
			m.wrappedPrefix += "\n" + strings.Join(parts, "\n")
		}
		m.wrappedPrefix += "\n"
	}
	m.chatLines = append(m.chatLines, line)
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// appendToLastLine appends text to the last line in the chat buffer.
func (m *Model) appendToLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.appendChatLine(text)
		return
	}
	// Only the last line changes — prefix stays unchanged.
	m.chatLines[len(m.chatLines)-1] += text
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// replaceLastLine replaces the last line in the chat buffer.
func (m *Model) replaceLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.chatLines = append(m.chatLines, text)
	} else {
		m.chatLines[len(m.chatLines)-1] = text
	}
	// Prefix is unchanged; only the last line may have been replaced.
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamToken(token string) {
	m.closeThinkingBlock()
	if m.streamAssistantBuf.Len() == 0 {
		label := AssistantStyle.Render(assistantLabel)
		m.streamAssistantLine = len(m.chatLines)
		m.appendChatLine(label + " ")
	}
	m.streamAssistantBuf.WriteString(token)
	m.appendToStreamLine(m.streamAssistantLine, token)
}

func (m *Model) handleStreamThinking(token string) {
	// Guard: if tool calls are already in progress, thinking tokens belong
	// before them (OpenAI protocol ensures this ordering).  Silently ignore
	// post-tool-call thinking to avoid placing it below tool call lines.
	if len(m.streamToolCallNames) > 0 {
		return
	}
	m.streamThinkingBuf.WriteString(token)

	if !m.streamThinkingOpen {
		m.streamThinkingOpen = true
		m.streamThinkingLine = len(m.chatLines)
		m.appendChatLine(ThinkingTagStyle.Render("<thinking>"))
	}

	// Rebuild the thinking line cleanly from the accumulated buffer.  This
	// avoids two problems with per-delta append+style: (1) interleaved
	// \x1b[0m codes destabilise word-wrap (line height jumps when the block
	// closes and normalises the styling), and (2) whitespace-only tokens
	// that the batcher splits into standalone segments are silently dropped
	// from the display by TrimSpace, only to re-appear after close.
	// When the buffer ends with a newline, wrapLine splits it into an extra
	// blank visual line that will flash away when the next token fills it in
	// or when </thinking> replaces it on close.  Trim trailing newlines so
	// the streaming display stays stable.
	displayBuf := strings.TrimRight(m.streamThinkingBuf.String(), "\n")
	m.replaceStreamLine(m.streamThinkingLine, ThinkingStyle.Render("<thinking>"+displayBuf))
}

// closeThinkingBlock finalizes an open thinking line in place (not necessarily
// the last chat line — assistant text or tool calls may have been appended).
func (m *Model) closeThinkingBlock() {
	if !m.streamThinkingOpen {
		return
	}
	m.streamThinkingOpen = false
	var line string
	if m.streamThinkingBuf.Len() > 0 {
		line = ThinkingTagStyle.Render("<thinking>" + m.streamThinkingBuf.String() + "</thinking>")
	} else {
		line = ThinkingTagStyle.Render("<thinking></thinking>")
	}
	m.replaceStreamLine(m.streamThinkingLine, line)
	m.streamThinkingBuf.Reset()
	m.streamThinkingLine = -1
	// Reset assistant state so content tokens arriving after this thinking
	// block create a new line below it, preserving temporal order.
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
}

// appendToStreamLine appends text to a tracked streaming line. When that line
// is last, uses the cheap prefix rebuild; otherwise full rewrap.
func (m *Model) appendToStreamLine(lineIdx int, text string) {
	if lineIdx < 0 || lineIdx >= len(m.chatLines) {
		m.appendToLastLine(text)
		return
	}
	m.chatLines[lineIdx] += text
	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

func (m *Model) replaceStreamLine(lineIdx int, text string) {
	if lineIdx < 0 || lineIdx >= len(m.chatLines) {
		m.replaceLastLine(text)
		return
	}
	m.chatLines[lineIdx] = text
	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamToolCall(index int, id string, name string) {
	// Close thinking if open — finalize the block in chat
	m.closeThinkingBlock()
	// Close assistant buffer (text tokens before tool call are shown as-is in chat)
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamToolCallNames[index] = name
	m.streamToolCallArgs[index] = ""
	m.streamToolCallIDs[index] = id
	m.streamToolCallLines[index] = len(m.chatLines) // appendChatLine will add at this index
	prefix := ToolCallStyle.Render("  →")
	m.appendChatLine(prefix + " " + name)
}

func (m *Model) handleStreamToolArgs(index int, id string, delta string) {
	lineIdx, ok := m.streamToolCallLines[index]
	if !ok || lineIdx < 0 || lineIdx >= len(m.chatLines) {
		return
	}
	m.streamToolCallArgs[index] += delta
	raw := m.streamToolCallArgs[index]

	// For patch_file: progressively render diff content as it streams in.
	// This avoids a jarring "pop-up" of the entire diff block in the result.
	if m.streamToolCallNames[index] == "patch_file" {
		if diff, ok := extractDiffValue(raw); ok && diff != "" {
			rendered := renderDiff(diff)
			renderedLines := strings.Split(rendered, "\n")
			prevCount := m.streamToolDiffCount[index]
			newCount := len(renderedLines)

			// First time we're showing diff lines: add the top border
			if prevCount == 0 {
				m.chatLines = append(m.chatLines, DiffMetaStyle.Render("  ╭─ diff ─"))
				m.streamToolDiffStart[index] = len(m.chatLines)
			}

			diffStart := m.streamToolDiffStart[index]
			// Update existing diff lines (content may have grown within a line)
			for i := 0; i < prevCount && i < newCount; i++ {
				m.chatLines[diffStart+i] = "  "+renderedLines[i]
			}
			// Append new diff lines
			for i := prevCount; i < newCount; i++ {
				m.chatLines = append(m.chatLines, "  "+renderedLines[i])
			}
			m.streamToolDiffCount[index] = newCount

			if lineIdx == len(m.chatLines)-1 {
				m.buildFromPrefix()
			} else {
				m.setViewportContent()
			}
			m.viewport.GotoBottom()
		}
		// Don't try to parse JSON for the args line; diff values are huge and
		// formatToolArgs would just truncate them. Show a clean compact line.
		prefix := ToolCallStyle.Render("  →")
		shortArgs := formatArgsCompact(raw, 120)
		toolName := m.streamToolCallNames[index]
		if shortArgs == "" {
			m.chatLines[lineIdx] = prefix + " " + toolName
		} else {
			m.chatLines[lineIdx] = prefix + " " + toolName + " " + ToolCallArgsStyle.Render(shortArgs)
		}
		if lineIdx == len(m.chatLines)-1 {
			m.buildFromPrefix()
		}
		m.viewport.GotoBottom()
		return
	}

	// Only show args once JSON is fully parseable.  Raw / truncated JSON
	// varies in length enough to cause the line to re-wrap and make
	// content below jump when handleStreamToolCallFinal normalises it.
	args, parseErr := parseInlineJSONArgs(raw)
	if parseErr != nil {
		return
	}

	// Format the same way handleStreamToolCallFinal does, so the line is
	// already in its final form when that fires.  This eliminates the jump
	// entirely for multi-key args and only leaves the minimal name→name+args
	// transition for tools whose last arg key completes the JSON.
	if len(args) == 0 {
		return
	}
	argStr := formatToolArgs(args)

	// Rebuild the line cleanly from the accumulated buffer so there is a
	// single contiguous SGR wrapper.  Per-delta styling produces interleaved
	// \x1b[0m sequences that destabilise word-wrap, causing the line height
	// to jump when handleStreamToolCallFinal normalises the styling.
	name := m.streamToolCallNames[index]
	prefix := ToolCallStyle.Render("  →")
	m.chatLines[lineIdx] = prefix + " " + name + " " + ToolCallArgsStyle.Render(argStr)

	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

// handleStreamToolCallFinal replaces the streaming tool call line with the final
// cleanly-formatted args (from the fully-parsed ToolCall).
func (m *Model) handleStreamToolCallFinal(index int, tc llm.ToolCall) {
	name, ok := m.streamToolCallNames[index]
	if !ok {
		return
	}
	lineIdx, ok := m.streamToolCallLines[index]
	if !ok || lineIdx < 0 || lineIdx >= len(m.chatLines) {
		return
	}
	prefix := ToolCallStyle.Render("  →")
	argStr := formatToolArgs(tc.Args)
	if argStr == "" {
		m.chatLines[lineIdx] = prefix + " " + name
	} else {
		m.chatLines[lineIdx] = prefix + " " + name + " " + ToolCallArgsStyle.Render(argStr)
	}
	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()

	// Capture diff content for patch_file calls so we can render it in the result
	// (fallback if progressive rendering didn't cover the full diff).
	if tc.Name == "patch_file" {
		if diff, ok := tc.Args["diff"].(string); ok && diff != "" {
			m.toolCallDiffs[tc.ID] = diff
			// If we already rendered diff lines progressively, close the border
			// and mark so the result handler skips the block render.
			if m.streamToolDiffCount[index] > 0 {
				m.chatLines = append(m.chatLines, DiffMetaStyle.Render("  ╰───────"))
				m.buildFromPrefix()
				m.viewport.GotoBottom()
				m.toolDiffShown[tc.ID] = true
			}
		}
	}
}

// parseInlineJSONArgs attempts to parse incomplete streaming JSON args.
// Returns the parsed map on success; nil+error when JSON is not yet complete.
func parseInlineJSONArgs(raw string) (map[string]interface{}, error) {
	s := strings.TrimSpace(raw)
	if s == "" || !strings.HasPrefix(s, "{") {
		return nil, fmt.Errorf("incomplete")
	}
	var args map[string]interface{}
	err := json.Unmarshal([]byte(s), &args)
	return args, err
}

func (m *Model) handleStreamToolResult(id string, name string, result string, success bool) {
	// Collect all new lines first, then append in a batch so the viewport
	// rebuilds once instead of on every appendChatLine call.
	var newLines []string

	status := "ok"
	statusStyle := ToolResultOKStyle
	if !success {
		status = "failed"
		statusStyle = ToolResultFailStyle
	}
	mark := ToolResultMarkStyle.Render("  ↳")
	newLines = append(newLines, fmt.Sprintf("%s %s  %s", mark, name, statusStyle.Render(status)))

	// For patch_file: render the original diff with colors
	showDiffResult := name == "show_diff" && isDiffContent(result)
	if name == "patch_file" {
		// If diff was already shown progressively during arg streaming,
		// skip the full block render.
		if m.toolDiffShown[id] {
			// Summary only — border was already closed in handleStreamToolCallFinal
			if m.verbose {
				for _, line := range strings.Split(result, "\n") {
					newLines = append(newLines, ToolResultBodyStyle.Render("  │ "+line))
				}
			} else {
				summary := summarizeResult(result, success)
				newLines = append(newLines, DimStyle.Render(fmt.Sprintf("  %s", summary)))
			}
		} else if diff, ok := m.toolCallDiffs[id]; ok && diff != "" {
			rendered := renderDiff(diff)
			if rendered != "" {
				newLines = append(newLines, DiffMetaStyle.Render("  ╭─ diff ─"))
				for _, line := range strings.Split(rendered, "\n") {
					newLines = append(newLines, line)
				}
				newLines = append(newLines, DiffMetaStyle.Render("  ╰───────"))
			}
		}
	} else if showDiffResult {
		rendered := renderDiff(result)
		if rendered != "" {
			newLines = append(newLines, DiffMetaStyle.Render("  ╭─ diff ─"))
			for _, line := range strings.Split(rendered, "\n") {
				newLines = append(newLines, line)
			}
			newLines = append(newLines, DiffMetaStyle.Render("  ╰───────"))
		}
	} else if m.verbose {
		for _, line := range strings.Split(result, "\n") {
			newLines = append(newLines, ToolResultBodyStyle.Render("  │ "+line))
		}
	} else {
		summary := summarizeResult(result, success)
		newLines = append(newLines, DimStyle.Render(fmt.Sprintf("  %s", summary)))
	}

	// If this already came from a diff path, stop — don't double-append.
	if showDiffResult {
		m.appendChatLines(newLines)
		return
	}

	m.appendChatLines(newLines)
}

// appendChatLines adds multiple lines to chat and rebuilds the viewport once.
func (m *Model) appendChatLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	for _, line := range lines {
		if len(m.chatLines) > 0 {
			parts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
			if m.wrappedPrefix == "" {
				m.wrappedPrefix = strings.Join(parts, "\n")
			} else {
				m.wrappedPrefix += "\n" + strings.Join(parts, "\n")
			}
			m.wrappedPrefix += "\n"
		}
		m.chatLines = append(m.chatLines, line)
	}
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamStart() {
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamThinkingLine = -1
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.streamToolCallLines = make(map[int]int)
	m.toolCallDiffs = make(map[string]string)
	m.streamToolDiffCount = make(map[int]int)
	m.streamToolDiffStart = make(map[int]int)
	m.toolDiffShown = make(map[string]bool)
}

func (m *Model) handleStreamRoundStart() {
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamThinkingLine = -1
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.streamToolCallLines = make(map[int]int)
	m.toolCallDiffs = make(map[string]string)
	m.streamToolDiffCount = make(map[int]int)
	m.streamToolDiffStart = make(map[int]int)
	// Keep toolDiffShown across rounds — each diff is shown once per turn
}

func (m *Model) handleStreamRoundEnd() {
	// Trim trailing newlines from the assistant content line before
	// finalizing the round, so intermediate display doesn't have trailing
	// blank lines.
	if m.streamAssistantLine >= 0 && m.streamAssistantLine < len(m.chatLines) {
		m.chatLines[m.streamAssistantLine] = strings.TrimRight(m.chatLines[m.streamAssistantLine], "\n")
	}
	m.closeThinkingBlock()
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	// Keep streamToolCallNames / toolCallDiffs until OnRoundStart or turn end so
	// OnToolCall finals and patch diffs still resolve after OnStreamEnd.

	m.setViewportContent()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamEnd() {
	// Trim trailing newlines from the assistant content line before
	// finalizing, so the display doesn't end with a blank line.
	if m.streamAssistantLine >= 0 && m.streamAssistantLine < len(m.chatLines) {
		m.chatLines[m.streamAssistantLine] = strings.TrimRight(m.chatLines[m.streamAssistantLine], "\n")
	}
	m.closeThinkingBlock()
	m.dismissApproval(false)
	m.streaming = false
	m.clearProgress()
	// ContextStats is read-only and local (no provider I/O) — safe on the
	// Update thread once StreamProcessInput has returned.
	m.refreshContextStats()
	if m.agent != nil {
		if err := m.agent.ConsumePersistError(); err != nil {
			m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", err)
		}
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
}

// refocusInput restarts the textarea cursor blink after streaming (blink ticks
// are ignored while streaming==true, so the blink loop must be restarted).
func (m *Model) refocusInput() tea.Cmd {
	if m.focus != FocusInput || m.modal != ModalNone {
		return nil
	}
	return m.textarea.Focus()
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

// requestContextStats refreshes the status-bar indicator asynchronously.
// Prefer refreshContextStats from Update handlers when the stream is idle.
func (m *Model) requestContextStats() tea.Cmd {
	if m.agent == nil {
		return nil
	}
	a := m.agent
	ctx := m.ctx
	return func() tea.Msg {
		if ctx == nil {
			ctx = context.Background()
		}
		return contextStatsMsg{stats: a.ContextStats(context.WithoutCancel(ctx))}
	}
}



func (m *Model) handleStreamError(err error) {
	wasStreaming := m.streaming
	m.streaming = false
	m.dismissApproval(false)
	m.clearProgress()
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamThinkingLine = -1
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.streamToolCallLines = make(map[int]int)
	m.toolCallDiffs = make(map[string]string)
	m.streamToolDiffCount = make(map[int]int)
	m.streamToolDiffStart = make(map[int]int)
	m.toolDiffShown = make(map[string]bool)
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
