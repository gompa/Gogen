package tui

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
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
	focus       FocusTarget
	modal       ModalKind
	streamCancel  context.CancelFunc // cancels the current streaming LLM call
	streaming   bool
	verbose     bool
	width       int
	height      int
	quitting    bool

	// Chat content buffer (lines to render)
	chatLines      []string
	wrappedPrefix  string // pre-wrapped content of chatLines[:len-1], ends with "\n" if non-empty
	wrappedContent string // pre-computed wrapped content for viewport
	styledLines    []string // wrappedContent split by "\n" (cached for selection render)

	// Streaming accumulation
	streamAssistantBuf  strings.Builder
	streamThinkingBuf   strings.Builder
	streamThinkingOpen  bool
	streamToolCallNames map[int]string // index -> name
	streamToolCallArgs  map[int]string // index -> accumulated args deltas
	streamToolCallIDs   map[int]string // index -> call ID (for correlating results)
	toolCallDiffs       map[string]string // call ID -> diff text (for patch_file/show_diff)

	// Input history
	inputHistory     []string
	historyIdx       int
	historyDraft     string

	// Context stats
	contextStats   agent.TurnContext
	contextLine    string

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
	sessionList    []agent.SessionInfo
	modelList      []llm.ModelInfo
	sessionCursor  int

	// Keymap
	keys           KeyMap

	// Screen dimensions tracking
	ready          bool

	// Text selection (mouse drag-to-select in viewport)
	selectionYOff int // viewport YOffset at selection start (for stable coordinates)
	selection    *SelectionState
	wrappedLines []string // ANSI-stripped wrapped content lines (coordinate mapping)
	wrappedLinesDirty bool
	statusMsg    string   // transient message (e.g. "Copied N chars")
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
		agent:             a,
		cfg:               cfg,
		viewport:          vp,
		textarea:          ta,
		focus:             FocusInput,
		modal:             ModalNone,
		streaming:         false,
		verbose:           verbose,
		chatLines:         make([]string, 0),
		streamToolCallNames: make(map[int]string),
		streamToolCallArgs:  make(map[int]string),
		streamToolCallIDs:   make(map[int]string),
		toolCallDiffs:       make(map[string]string),
		keys:              DefaultKeyMap,
		sessionID:         "",
		approvalResult:    make(chan bool, 1),
		selectionYOff:     -1,
		selection:         nil,
		wrappedLines:      nil,
		statusMsg:         "",
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
func (m *Model) wrapLine(line string) []string {
	wrapped := wordwrap.String(line, m.wrapWidth())
	parts := strings.Split(wrapped, "\n")
	if len(parts) > 1 {
		sgr := extractLeadingSGR(line)
		if sgr != "" && !strings.Contains(parts[0], "\x1b[0m") {
			for i := 1; i < len(parts); i++ {
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

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		// Clear transient status message on any key press
		if m.statusMsg != "" {
			m.statusMsg = ""
		}
		// Global hotkeys that work regardless of focus/modal
		switch {
		case key.Matches(msg, m.keys.ForceQuit):
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
		return m, nil

	case streamRoundStartMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamRoundStart()
		return m, nil

	case streamTokenMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToken(msg.token)
		return m, nil

	case streamThinkingMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamThinking(msg.token)
		return m, nil

	case streamToolCallMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolCall(msg.index, msg.id, msg.name)
		return m, nil

	case streamToolCallArgsMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolArgs(msg.index, msg.id, msg.delta)
		return m, nil

	case streamToolCallFinalMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolCallFinal(msg.index, msg.tc)
		return m, nil

	case streamToolResultMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamToolResult(msg.id, msg.name, msg.result, msg.success)
		return m, nil

	case streamRoundEndMsg:
		if !m.streaming {
			return m, nil
		}
		m.handleStreamRoundEnd()
		return m, nil

	case streamEndMsg:
		// Always process – resets streaming state
		m.handleStreamEnd()
		return m, nil

	case streamErrorMsg:
		m.handleStreamError(msg.err)
		return m, nil

	case contextStatsMsg:
		// Always process – may arrive after streamEndMsg
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
		inputArea = DimStyle.Render("  … processing …")
	} else {
		inputArea = m.textarea.View()
	}

	// Divider with focus indicator
	var divider string
	if m.focus == FocusViewport {
		indicator := " [SCROLL] Press i or Esc to return to input "
		line := strings.Repeat("─", m.width)
		divider = DividerStyle.Render(line[:max(0,m.width-len(indicator))] + indicator)
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
	// Close thinking block if open — finalize it properly
	if m.streamThinkingOpen {
		m.streamThinkingOpen = false
		if m.streamThinkingBuf.Len() > 0 {
			m.replaceLastLine(ThinkingTagStyle.Render("<thinking>" + m.streamThinkingBuf.String() + "</thinking>"))
		} else {
			m.replaceLastLine(ThinkingTagStyle.Render("<thinking></thinking>"))
		}
		m.streamThinkingBuf.Reset()
	}
	if m.streamAssistantBuf.Len() == 0 {
		label := AssistantStyle.Render(assistantLabel)
		m.appendChatLine(label + " ")
	}
	m.streamAssistantBuf.WriteString(token)
	// Append just the delta token rather than rebuilding the full line from
	// the buffer; avoids O(n²) String() copy for long responses.
	m.appendToLastLine(token)
}

func (m *Model) handleStreamThinking(token string) {
	if !m.streamThinkingOpen {
		m.streamThinkingOpen = true
		m.appendChatLine(ThinkingTagStyle.Render("<thinking>"))
	}
	m.streamThinkingBuf.WriteString(token)
	m.appendToLastLine(ThinkingStyle.Render(token))
}

func (m *Model) handleStreamToolCall(index int, id string, name string) {
	// Close thinking if open — finalize the block in chat
	if m.streamThinkingOpen {
		m.streamThinkingOpen = false
		if m.streamThinkingBuf.Len() > 0 {
			m.replaceLastLine(ThinkingTagStyle.Render("<thinking>" + m.streamThinkingBuf.String() + "</thinking>"))
		}
		m.streamThinkingBuf.Reset()
	}
	// Close assistant buffer (text tokens before tool call are shown as-is in chat)
	m.streamAssistantBuf.Reset()
	m.streamToolCallNames[index] = name
	m.streamToolCallArgs[index] = ""
	m.streamToolCallIDs[index] = id
	prefix := ToolCallStyle.Render("  →")
	m.appendChatLine(prefix + " " + name + " ")
}

func (m *Model) handleStreamToolArgs(index int, id string, delta string) {
	_, ok := m.streamToolCallNames[index]
	if !ok {
		return
	}
	m.streamToolCallArgs[index] += delta
	// Append just the styled delta — avoid rebuilding the full line per token.
	m.appendToLastLine(ToolCallArgsStyle.Render(delta))
}

// handleStreamToolCallFinal replaces the streaming tool call line with the final
// cleanly-formatted args (from the fully-parsed ToolCall).
func (m *Model) handleStreamToolCallFinal(index int, tc llm.ToolCall) {
	name, ok := m.streamToolCallNames[index]
	if !ok {
		return
	}
	prefix := ToolCallStyle.Render("  →")
	argStr := formatToolArgs(tc.Args)
	if argStr == "" {
		m.replaceLastLine(prefix + " " + name)
	} else {
		m.replaceLastLine(prefix + " " + name + " " + ToolCallArgsStyle.Render(argStr))
	}
	// Capture diff content for patch_file calls so we can render it colored in the result
	if tc.Name == "patch_file" {
		if diff, ok := tc.Args["diff"].(string); ok && diff != "" {
			m.toolCallDiffs[tc.ID] = diff
		}
	}
}

func (m *Model) handleStreamToolResult(id string, name string, result string, success bool) {
	status := "ok"
	statusStyle := ToolResultOKStyle
	if !success {
		status = "failed"
		statusStyle = ToolResultFailStyle
	}
	mark := ToolResultMarkStyle.Render("  ↳")
	m.appendChatLine(fmt.Sprintf("%s %s  %s", mark, name, statusStyle.Render(status)))

	// For patch_file: render the original diff with colors
	if name == "patch_file" {
		if diff, ok := m.toolCallDiffs[id]; ok && diff != "" {
			m.appendChatLine(DiffMetaStyle.Render("  ╭─ diff ─"))
			for _, line := range strings.Split(renderDiff(diff), "\n") {
				m.appendChatLine(line)
			}
			m.appendChatLine(DiffMetaStyle.Render("  ╰───────"))
		}
	}

	// For show_diff: render the result as a colored diff if it looks like one
	if name == "show_diff" && isDiffContent(result) {
		m.appendChatLine(DiffMetaStyle.Render("  ╭─ diff ─"))
		for _, line := range strings.Split(renderDiff(result), "\n") {
			m.appendChatLine(line)
		}
		m.appendChatLine(DiffMetaStyle.Render("  ╰───────"))
		return
	}

	// Standard output: verbose shows body, compact shows summary
	if m.verbose {
		for _, line := range strings.Split(result, "\n") {
			m.appendChatLine(ToolResultBodyStyle.Render("  │ " + line))
		}
	} else {
		summary := summarizeResult(result, success)
		m.appendChatLine(DimStyle.Render(fmt.Sprintf("  %s", summary)))
	}
}

func (m *Model) handleStreamStart() {
	m.streamAssistantBuf.Reset()
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.toolCallDiffs = make(map[string]string)
}

func (m *Model) handleStreamRoundStart() {
	m.streamAssistantBuf.Reset()
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.toolCallDiffs = make(map[string]string)
}

func (m *Model) handleStreamRoundEnd() {
	if m.streamThinkingOpen {
		m.streamThinkingOpen = false
		if m.streamThinkingBuf.Len() > 0 {
			m.replaceLastLine(ThinkingTagStyle.Render("<thinking>" + m.streamThinkingBuf.String() + "</thinking>"))
		} else if len(m.chatLines) > 0 {
			m.replaceLastLine(ThinkingTagStyle.Render("<thinking></thinking>"))
		}
	}
	m.streamThinkingBuf.Reset()
	m.streamAssistantBuf.Reset()
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.toolCallDiffs = make(map[string]string)

	m.setViewportContent()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamEnd() {
	m.streaming = false

	// Update context stats
	if m.agent != nil {
		stats := m.agent.ContextStats(m.ctx)
		if line := agent.FormatContextBrief(stats); line != "" {
			m.contextLine = line
			m.contextStats = stats
		}
	}

	m.setViewportContent()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamError(err error) {
	m.streaming = false
	m.streamAssistantBuf.Reset()
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.toolCallDiffs = make(map[string]string)
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Error: %v", err)))
	}
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
