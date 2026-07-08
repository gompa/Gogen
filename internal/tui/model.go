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
	streaming   bool
	verbose     bool
	width       int
	height      int
	quitting    bool

	// Chat content buffer (lines to render)
	chatLines      []string
	wrappedContent string // pre-computed wrapped content for viewport

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

	// Keymap
	keys           KeyMap

	// Screen dimensions tracking
	ready          bool

	// Text selection (mouse drag-to-select in viewport)
	selectionYOff int // viewport YOffset at selection start (for stable coordinates)
	selection    *SelectionState
	wrappedLines []string // ANSI-stripped wrapped content lines (coordinate mapping)
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

// setViewportContent wraps chatLines to the viewport width and sets the content.
func (m *Model) setViewportContent() {
	if m.width <= 2 {
		return
	}
	w := m.viewport.Width
	if w < 10 {
		w = 10
	}
	w -= m.viewport.Style.GetHorizontalFrameSize()
	if w < 10 {
		w = 10
	}
	raw := strings.Join(m.chatLines, "\n")
	m.wrappedContent = wordwrap.String(raw, w)
	// Store plain (ANSI-stripped) copy for selection coordinate mapping
	m.wrappedLines = strings.Split(stripANSI(m.wrappedContent), "\n")
	// Content changed — clear any stale selection
	m.clearSelection()
	m.viewport.SetContent(m.wrappedContent)
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
		m.handleStreamStart()
		return m, nil

	case streamRoundStartMsg:
		m.handleStreamRoundStart()
		return m, nil

	case streamTokenMsg:
		m.handleStreamToken(msg.token)
		return m, nil

	case streamThinkingMsg:
		m.handleStreamThinking(msg.token)
		return m, nil

	case streamToolCallMsg:
		m.handleStreamToolCall(msg.index, msg.id, msg.name)
		return m, nil

	case streamToolCallArgsMsg:
		m.handleStreamToolArgs(msg.index, msg.id, msg.delta)
		return m, nil

	case streamToolCallFinalMsg:
		m.handleStreamToolCallFinal(msg.index, msg.tc)
		return m, nil

	case streamToolResultMsg:
		m.handleStreamToolResult(msg.id, msg.name, msg.result, msg.success)
		return m, nil

	case streamRoundEndMsg:
		m.handleStreamRoundEnd()
		return m, nil

	case streamEndMsg:
		m.handleStreamEnd()
		return m, nil

	case streamErrorMsg:
		m.handleStreamError(msg.err)
		return m, nil

	case contextStatsMsg:
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

	// Approval result (from goroutine)
	case approvalResultMsg:
		m.approvalResult <- msg.approved
		m.modal = ModalNone
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

	return lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a1a")).
		Render(strings.TrimRight(b.String(), "\n"))
}

// appendChatLine adds a line to the chat buffer and updates the viewport.
func (m *Model) appendChatLine(line string) {
	m.chatLines = append(m.chatLines, line)
	m.setViewportContent()
	m.viewport.GotoBottom()
}

// appendToLastLine appends text to the last line in the chat buffer.
func (m *Model) appendToLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.chatLines = append(m.chatLines, text)
	} else {
		m.chatLines[len(m.chatLines)-1] += text
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
}

// replaceLastLine replaces the last line in the chat buffer.
func (m *Model) replaceLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.chatLines = append(m.chatLines, text)
	} else {
		m.chatLines[len(m.chatLines)-1] = text
	}
	m.setViewportContent()
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
	m.replaceLastLine(AssistantStyle.Render(assistantLabel) + " " + m.streamAssistantBuf.String())
}

func (m *Model) handleStreamThinking(token string) {
	if !m.streamThinkingOpen {
		m.streamThinkingOpen = true
		m.appendChatLine(ThinkingTagStyle.Render("<thinking>"))
	}
	m.streamThinkingBuf.WriteString(token)
	m.replaceLastLine(ThinkingTagStyle.Render("<thinking>" + m.streamThinkingBuf.String()))
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
	name := m.streamToolCallNames[index]
	m.streamToolCallArgs[index] += delta
	prefix := ToolCallStyle.Render("  →")
	m.replaceLastLine(prefix + " " + name + " " + ToolCallArgsStyle.Render(m.streamToolCallArgs[index]))
}

// handleStreamToolCallFinal replaces the streaming tool call line with the final
// cleanly-formatted args (from the fully-parsed ToolCall).
func (m *Model) handleStreamToolCallFinal(index int, tc llm.ToolCall) {
	name := m.streamToolCallNames[index]
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
		stats := m.agent.ContextStats(context.Background())
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
			first = first[:117] + "..."
		}
		return fmt.Sprintf("%s (%d chars)", first, chars)
	}
	if lines == 1 && chars <= 120 {
		return trimmed
	}
	return fmt.Sprintf("(%d lines, %d chars)", lines, chars)
}
