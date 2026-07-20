package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"gogen/internal/agent"
	"gogen/internal/session"

	tea "github.com/charmbracelet/bubbletea"
)

// renderModal renders the currently active modal.
func (m *Model) renderModal() string {
	switch m.modal {
	case ModalApproval:
		return m.renderApprovalModal()
	case ModalSessions:
		return m.renderSessionsModal()
	case ModalModels:
		return m.renderModelsModal()
	case ModalHelp:
		return m.renderHelpModal()
	case ModalCompletion:
		return m.renderCompletionModal()
	}
	return ""
}

// handleModalKey dispatches keys when a modal is active.
func (m *Model) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.modal {
	case ModalApproval:
		return m.handleApprovalKey(msg)
	case ModalSessions:
		return m.handleSessionsKey(msg)
	case ModalModels:
		return m.handleModelsKey(msg)
	case ModalHelp:
		return m.handleHelpKey(msg)
	case ModalCompletion:
		return m.handleCompletionKey(msg)
	}
	return m, nil
}

// --- Approval Modal ---

type approvalUIState struct {
	paths   []string
	reason  string
	cursor  int // 0 = No, 1 = Yes
}

func (m *Model) renderApprovalModal() string {
	if m.approvalUI == nil {
		return ""
	}

	var rows []styleLine

	// Title
	rows = append(rows, styleLine{text: "Delete Approval Required", highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	// Reason
	reason := fmt.Sprintf("Reason: %s", m.approvalUI.reason)
	rows = append(rows, styleLine{text: reason, highlight: false})
	rows = append(rows, styleLine{text: "", highlight: false})

	// File list
	rows = append(rows, styleLine{text: "Files to delete:", preStyled: true})
	for _, p := range m.approvalUI.paths {
		rows = append(rows, styleLine{text: "  • " + p, preStyled: true})
	}
	rows = append(rows, styleLine{text: "", preStyled: true})

	// Prompt + buttons (pre-styled line with inline highlights)
	noStyle := ansiDimOn
	yesStyle := ansiDimOn
	if m.approvalUI.cursor == 0 {
		noStyle = ansiHighlightOn
	}
	if m.approvalUI.cursor == 1 {
		yesStyle = ansiHighlightOn
	}
	promptLine := ansiPromptOn + "Allow delete?" + ansiReset +
		"  [" + noStyle + "No" + ansiReset +
		"]  [" + yesStyle + "Yes" + ansiReset + "]"
	rows = append(rows, styleLine{text: promptLine, preStyled: true})

	return renderBorderedModal(rows)
}

func (m *Model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "h":
		if m.approvalUI.cursor > 0 {
			m.approvalUI.cursor--
		}
	case "right", "l":
		if m.approvalUI.cursor < 1 {
			m.approvalUI.cursor++
		}
	case "enter", "y":
		m.dismissApproval(true)
		return m, nil
	case "n", "esc":
		m.dismissApproval(false)
		return m, nil
	case "ctrl+c":
		if m.streamCancel != nil {
			m.streamCancel()
		}
		m.dismissApproval(false)
		return m, nil
	}
	return m, nil
}

// dismissApproval closes the approval modal and sends a non-blocking result.
func (m *Model) dismissApproval(approved bool) {
	// Only honour a dismissal if a request is currently in flight. Without
	// this guard a stale modal-dismissal (e.g. user pressed ctrl+c after the
	// stream was cancelled) could write into the shared channel and either
	// leak as a stuck approver waiting on a value that will never arrive, or
	// be silently dropped, depending on timing.
	if m.modal != ModalApproval && m.approvalUI == nil {
		return
	}
	m.approvalUI = nil
	m.modal = ModalNone
	if m.approvalResult == nil || !m.approvalInFlight {
		return
	}
	m.approvalInFlight = false
	select {
	case m.approvalResult <- approved:
	default:
		// Buffer full or no waiter — drop rather than block the UI thread.
	}
}

// --- Sessions Modal ---

func (m *Model) renderSessionsModal() string {
	if len(m.sessionList) == 0 {
		return renderBorderedModal([]styleLine{
			{text: "Saved Sessions", highlight: true},
			{text: "", highlight: false},
			{text: "No saved sessions.", highlight: false},
			{text: "", highlight: false},
			{text: "Press esc to close", highlight: false},
		})
	}

	// Constrain visible area to fit the terminal.
	reserved := 13 // border(2) + title(2) + footer(4) + top/bottom margin(5)
	maxVisible := max(3, m.height-reserved)

	// Clamp cursor.
	if m.sessionCursor >= len(m.sessionList) {
		m.sessionCursor = len(m.sessionList) - 1
	}
	if m.sessionCursor < 0 {
		m.sessionCursor = 0
	}

	// Compute scroll window so cursor stays visible.
	start := 0
	if len(m.sessionList) > maxVisible {
		start = m.sessionCursor - maxVisible/2
		if start < 0 {
			start = 0
		}
		if start > len(m.sessionList)-maxVisible {
			start = len(m.sessionList) - maxVisible
		}
	}
	end := start + maxVisible
	if end > len(m.sessionList) {
		end = len(m.sessionList)
	}

	var rows []styleLine

	// Title
	rows = append(rows, styleLine{text: "Saved Sessions", highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	// Overflow indicator (top)
	if start > 0 {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↑ %d more", start), highlight: false,
		})
	}

	// Session entries
	for i := start; i < end; i++ {
		s := m.sessionList[i]
		line := fmt.Sprintf("  %s  (%d msgs)", s.ID, s.MessageCount)
		if s.Label != "" {
			line += fmt.Sprintf("  %q", s.Label)
		}
		if s.ID == m.sessionID {
			line += "  ← current"
		}
		rows = append(rows, styleLine{
			text:      line,
			highlight: i == m.sessionCursor,
		})
	}

	// Overflow indicator (bottom)
	if end < len(m.sessionList) {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↓ %d more", len(m.sessionList)-end), highlight: false,
		})
	}

	// Footer
	rows = append(rows, styleLine{text: "", highlight: false})
	rows = append(rows, styleLine{
		text: "↑↓/jk navigate  pgup/pgdn  enter resume  d delete  esc close", highlight: false,
	})

	return renderBorderedModal(rows)
}

func (m *Model) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.modal = ModalNone
		return m, nil
	case "up", "k":
		if m.sessionCursor > 0 {
			m.sessionCursor--
		}
	case "down", "j":
		if m.sessionCursor < len(m.sessionList)-1 {
			m.sessionCursor++
		}
	case "pgup":
		page := max(1, m.height-20)
		m.sessionCursor -= page
		if m.sessionCursor < 0 {
			m.sessionCursor = 0
		}
	case "pgdown":
		page := max(1, m.height-20)
		m.sessionCursor += page
		if m.sessionCursor >= len(m.sessionList) {
			m.sessionCursor = len(m.sessionList) - 1
		}
	case "home", "g":
		m.sessionCursor = 0
	case "end", "G":
		m.sessionCursor = len(m.sessionList) - 1
	case "enter":
		if len(m.sessionList) > 0 && m.sessionCursor >= 0 && m.sessionCursor < len(m.sessionList) {
			return m.resumeSelectedSession()
		}
	case "d":
		if len(m.sessionList) > 0 && m.sessionCursor >= 0 && m.sessionCursor < len(m.sessionList) {
			return m.deleteSelectedSession()
		}
	}
	return m, nil
}

// --- Bordered modal helpers ---

type styleLine struct {
	text      string
	highlight bool // if true, apply ansiHighlightOn; if false, ansiDimOn
	preStyled bool // if true, text already contains ANSI; don't add prefix
	prompt    bool // if true and preStyled, use ansiPromptOn instead of highlight/dim
}

// renderBorderedModal draws a plain border box around styled text lines.
// Uses ansiHighlightOn / ansiDimOn + ansiReset (foreground-only reset,
// never touches background) so the overlay's #1a1a1a background survives
// through the entire line, including right-hand padding.
// When preStyled is true the line is emitted as-is (caller embeds ANSI).
func renderBorderedModal(rows []styleLine) string {
	// Compute max visible width of plain text (strip ANSI for measurement).
	maxW := 0
	for _, r := range rows {
		w := lipgloss.Width(r.text)
		if w > maxW {
			maxW = w
		}
	}
	innerW := maxW + 4

	top := "╭" + strings.Repeat("─", innerW) + "╮"
	bot := "╰" + strings.Repeat("─", innerW) + "╯"

	var b strings.Builder
	b.WriteString(top)
	b.WriteByte('\n')
	for _, r := range rows {
		b.WriteString("│  ")
		if r.preStyled {
			// Caller already embedded ANSI — emit verbatim.
			b.WriteString(r.text)
		} else {
			if r.highlight {
				b.WriteString(ansiHighlightOn)
			} else {
				b.WriteString(ansiDimOn)
			}
			b.WriteString(r.text)
			b.WriteString(ansiReset)
		}
		// Fill remaining content width after text.
		pad := maxW - lipgloss.Width(r.text)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString("  │")
		b.WriteByte('\n')
	}
	b.WriteString(bot)
	return b.String()
}

// --- Models Modal ---

func (m *Model) renderModelsModal() string {
	if len(m.modelList) == 0 {
		return renderBorderedModal([]styleLine{
			{text: "Available Models", highlight: true},
			{text: "", highlight: false},
			{text: "No models available.", highlight: false},
			{text: "", highlight: false},
			{text: "Press esc to close", highlight: false},
		})
	}

	// Constrain visible area to fit the terminal.
	reserved := 13 // border(2) + title(2) + footer(4) + top/bottom margin(5)
	maxVisible := max(3, m.height-reserved)

	// Clamp cursor.
	if m.modelCursor >= len(m.modelList) {
		m.modelCursor = len(m.modelList) - 1
	}
	if m.modelCursor < 0 {
		m.modelCursor = 0
	}

	// Compute scroll window so cursor stays visible.
	start := 0
	if len(m.modelList) > maxVisible {
		start = m.modelCursor - maxVisible/2
		if start < 0 {
			start = 0
		}
		if start > len(m.modelList)-maxVisible {
			start = len(m.modelList) - maxVisible
		}
	}
	end := start + maxVisible
	if end > len(m.modelList) {
		end = len(m.modelList)
	}

	// Build plain-text lines first so we can compute the max content width.
	var rows []styleLine

	// Title
	rows = append(rows, styleLine{text: "Available Models", highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	// Overflow indicator (top)
	if start > 0 {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↑ %d more", start), highlight: false,
		})
	}

	// Model entries
	for i := start; i < end; i++ {
		mdl := m.modelList[i]
		line := fmt.Sprintf("  %2d. %s", i+1, mdl.ID)
		if mdl.ContextLimit > 0 {
			line += fmt.Sprintf("  (context: %d tokens)", mdl.ContextLimit)
		}
		if mdl.Current {
			line += " *"
		}
		rows = append(rows, styleLine{
			text:      line,
			highlight: i == m.modelCursor,
		})
	}

	// Overflow indicator (bottom)
	if end < len(m.modelList) {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↓ %d more", len(m.modelList)-end), highlight: false,
		})
	}

	// Footer
	rows = append(rows, styleLine{text: "", highlight: false})
	rows = append(rows, styleLine{
		text: "↑↓/jk navigate  enter load  esc close", highlight: false,
	})

	return renderBorderedModal(rows)
}

func (m *Model) handleModelsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.modal = ModalNone
		return m, nil
	case "up", "k":
		if m.modelCursor > 0 {
			m.modelCursor--
		}
	case "down", "j":
		if m.modelCursor < len(m.modelList)-1 {
			m.modelCursor++
		}
	case "pgup":
		page := max(1, m.height-20)
		m.modelCursor -= page
		if m.modelCursor < 0 {
			m.modelCursor = 0
		}
	case "pgdown":
		page := max(1, m.height-20)
		m.modelCursor += page
		if m.modelCursor >= len(m.modelList) {
			m.modelCursor = len(m.modelList) - 1
		}
	case "enter":
		if len(m.modelList) > 0 && m.modelCursor >= 0 && m.modelCursor < len(m.modelList) {
			return m.loadSelectedModel()
		}
	}
	return m, nil
}

// loadSelectedModel switches to the model at modelCursor from the models modal.
func (m *Model) loadSelectedModel() (tea.Model, tea.Cmd) {
	mdl := m.modelList[m.modelCursor]
	// Skip if already on this model.
	if mdl.Current {
		m.modal = ModalNone
		return m, nil
	}
	if err := m.agent.SelectModel(m.ctx, mdl.ID); err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Models: %v", err)))
		m.modal = ModalNone
		return m, nil
	}
	limit := m.agent.ContextLimit()
	msg := fmt.Sprintf("Switched to model: %s", m.agent.CurrentModel())
	if limit > 0 {
		msg += fmt.Sprintf(" (context: %d tokens)", limit)
	}
	m.appendChatLine(SystemStyle.Render(msg))
	m.refreshContextStats()
	// Re-render chat lines with updated model line
	m.chatLines = renderMessages(m.agent.Messages, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())
	m.setViewportContent()
	m.viewport.GotoBottom()
	m.modal = ModalNone
	return m, nil
}

// --- Help Modal ---

func (m *Model) renderHelpModal() string {
	sections := []struct {
		title string
		binds [][]string
	}{
		{"Commands", [][]string{
			{"enter", "Submit input"},
			{"ctrl+c", "Cancel turn / Quit"},
			{"ctrl+\\", "Force quit"},
			{"F1 / /help", "Show this help"},
			{"ctrl+v", "Toggle verbose"},
		}},
		{"Input Editing", [][]string{
			{"ctrl+a / home", "Line start"},
			{"ctrl+e / end", "Line end"},
			{"ctrl+k", "Kill to end"},
			{"ctrl+u", "Kill to start"},
			{"ctrl+w", "Kill word backward"},
			{"ctrl+left/right", "Word left/right"},
			{"ctrl+d", "Delete forward / EOF quit"},
			{"backspace", "Delete backward"},
			{"tab", "Complete slash cmds"},
		}},
		{"Viewport (esc to focus)", [][]string{
			{"↑ ↓ j k", "Scroll line"},
			{"pgup / pgdn", "Scroll page"},
			{"home / end", "Top / bottom"},
			{"mouse wheel", "Scroll viewport"},
			{"i / enter", "Return to input"},
		}},
		{"Slash Commands", [][]string{
			{"/help", "Show this help"},
			{"/plan / /act", "Toggle plan/act mode"},
			{"/mode", "Show current mode"},
			{"/models", "List/switch models"},
			{"/context", "Context usage details"},
			{"/new", "Start new session"},
			{"/resume", "List/restore/delete sessions"},
			{"/compact", "Compact history"},
			{"/verbose", "Toggle verbose output"},
			{"/save-config", "Write config to .gogen/"},
			{"dir <path>", "Change working dir"},
			{"/exit", "Quit GoGen"},
		}},
		{"Text Selection", [][]string{
			{"click+drag", "Select text in viewport"},
			{"ctrl+shift+c", "Copy selection"},
			{"right click", "Cancel selection"},
			{"esc", "Dismiss modal / focus viewport"},
		}},
	}

	var rows []styleLine

	// Title
	rows = append(rows, styleLine{text: "Keybindings", highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	for _, sec := range sections {
		// Section header
		rows = append(rows, styleLine{text: sec.title, highlight: false})
		for _, bind := range sec.binds {
			key := bind[0]
			desc := bind[1]
			// Pre-style: cyan key, plain desc, 24-char key column
			keyCol := ansiCyanOn + key + ansiReset
			line := fmt.Sprintf("  %-24s %s", keyCol, desc)
			rows = append(rows, styleLine{text: line, preStyled: true})
		}
		rows = append(rows, styleLine{text: "", highlight: false})
	}

	// Footer
	rows = append(rows, styleLine{text: "any key to close", highlight: false})

	return renderBorderedModal(rows)
}

func (m *Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key dismisses the help overlay
	m.modal = ModalNone
	return m, nil
}

// --- Completion Modal ---

func (m *Model) renderCompletionModal() string {
	if len(m.completions) == 0 {
		return ""
	}
	// Build a single line with inline highlights separated by "  ".
	var b strings.Builder
	for i, c := range m.completions {
		if i == m.completionIdx {
			b.WriteString(ansiHighlightOn)
		} else {
			b.WriteString(ansiDimOn)
		}
		b.WriteString(c)
		b.WriteString(ansiReset)
		if i < len(m.completions)-1 {
			b.WriteString("  ")
		}
	}
	return renderBorderedModal([]styleLine{
		{text: b.String(), preStyled: true},
	})
}

func (m *Model) handleCompletionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modal = ModalNone
		m.completions = nil
		return m, nil
	case "tab":
		m.completionIdx = (m.completionIdx + 1) % len(m.completions)
		return m, nil
	case "shift+tab":
		m.completionIdx--
		if m.completionIdx < 0 {
			m.completionIdx = len(m.completions) - 1
		}
		return m, nil
	case "enter":
		if m.completionIdx >= 0 && m.completionIdx < len(m.completions) {
			prefix, _, ok := agent.ResumeLinePrefix(m.completionLine)
			if ok {
				newArg := m.completions[m.completionIdx]
				if newArg == "del" {
					newArg = "del "
				}
				m.textarea.Reset()
				m.textarea.SetValue(prefix + newArg)
				m.textarea.CursorEnd()
			} else if strings.HasPrefix(strings.TrimRight(m.completionLine, " \t"), "/") {
				m.textarea.Reset()
				m.textarea.SetValue(m.completions[m.completionIdx] + " ")
				m.textarea.CursorEnd()
			}
		}
		m.modal = ModalNone
		m.completions = nil
		return m, nil
	}
	return m, nil
}

// resumeSelectedSession resumes the session at sessionCursor from the sessions modal.
func (m *Model) resumeSelectedSession() (tea.Model, tea.Cmd) {
	id := m.sessionList[m.sessionCursor].ID
	// Skip if already on this session.
	if id == m.sessionID {
		m.modal = ModalNone
		return m, nil
	}
	result, _, err := m.agent.HandleSessionCommand(m.ctx, "resume "+id, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		m.modal = ModalNone
		return m, nil
	}
	if result.Action == agent.SessionActionClearChat {
		m.chatLines = nil
		m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
		if len(result.History) > 0 {
			m.chatLines = append(m.chatLines, renderMessages(result.History, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())...)
		}
		m.setViewportContent()
		m.viewport.GotoBottom()
		m.sessionID = m.agent.SessionID
		m.modal = ModalNone
		m.refreshContextStats()
		return m, nil
	}
	m.modal = ModalNone
	return m, nil
}

// deleteSelectedSession deletes the session at sessionCursor from the sessions modal.
func (m *Model) deleteSelectedSession() (tea.Model, tea.Cmd) {
	id := m.sessionList[m.sessionCursor].ID
	result, _, err := m.agent.HandleSessionCommand(m.ctx, "resume del "+id, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		m.modal = ModalNone
		return m, nil
	}
	m.appendChatLine(SystemStyle.Render(result.Output))
	// Remove from the local list.
	m.sessionList = append(m.sessionList[:m.sessionCursor], m.sessionList[m.sessionCursor+1:]...)
	if m.sessionCursor >= len(m.sessionList) {
		m.sessionCursor = len(m.sessionList) - 1
	}
	if m.sessionCursor < 0 {
		m.sessionCursor = 0
	}
	if len(m.sessionList) == 0 {
		m.modal = ModalNone
	}
	if result.Action == agent.SessionActionClearChat {
		// The deleted session was the *current* one; start a new session.
		m.chatLines = nil
		m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
		m.setViewportContent()
		m.viewport.GotoBottom()
		m.sessionID = m.agent.SessionID
		m.refreshContextStats()
		return m, nil
	}
	return m, nil
}
