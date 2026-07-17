package tui

import (
	"fmt"
	"strings"

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

	var b strings.Builder
	b.WriteString(ModalTitleStyle.Render("Delete Approval Required"))
	b.WriteString("\n\n")
	b.WriteString(ModalDimStyle.Render(fmt.Sprintf("Reason: %s", m.approvalUI.reason)))
	b.WriteString("\n\n")
	b.WriteString("Files to delete:\n")
	for _, p := range m.approvalUI.paths {
		b.WriteString(fmt.Sprintf("  • %s\n", p))
	}
	b.WriteString("\n")
	b.WriteString(ModalPromptStyle.Render("Allow delete?"))

	noStyle := ModalDimStyle
	yesStyle := ModalDimStyle
	if m.approvalUI.cursor == 0 {
		noStyle = ModalHighlightStyle
	}
	if m.approvalUI.cursor == 1 {
		yesStyle = ModalHighlightStyle
	}
	b.WriteString(fmt.Sprintf("  [%s]  [%s]", noStyle.Render("No"), yesStyle.Render("Yes")))

	return ModalBorderStyle.Render(b.String())
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
	if m.modal != ModalApproval && m.approvalUI == nil {
		return
	}
	m.approvalUI = nil
	m.modal = ModalNone
	if m.approvalResult == nil {
		return
	}
	select {
	case m.approvalResult <- approved:
	default:
		// Buffer full or no waiter — drop rather than block the UI thread.
	}
}

// --- Sessions Modal ---

func (m *Model) renderSessionsModal() string {
	var b strings.Builder
	b.WriteString(ModalTitleStyle.Render("Saved Sessions"))
	b.WriteString("\n\n")

	if len(m.sessionList) > 0 {
		// Constrain visible area to fit the terminal.
		// Reserve lines for border, title, footer, and margin.
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

		if start > 0 {
			b.WriteString(ModalDimStyle.Render(fmt.Sprintf("  ↑ %d more\n", start)))
		}

		for i := start; i < end; i++ {
			s := m.sessionList[i]
			line := fmt.Sprintf("  %s  (%d msgs)", s.ID, s.MessageCount)
			if s.Label != "" {
				line += fmt.Sprintf("  %q", s.Label)
			}
			if s.ID == m.sessionID {
				line += "  ← current"
			}
			if i == m.sessionCursor {
				b.WriteString(ModalHighlightStyle.Render(line))
			} else {
				b.WriteString(ModalDimStyle.Render(line))
			}
			b.WriteString("\n")
		}

		if end < len(m.sessionList) {
			b.WriteString(ModalDimStyle.Render(fmt.Sprintf("  ↓ %d more\n", len(m.sessionList)-end)))
		}
	} else {
		b.WriteString(ModalDimStyle.Render("No saved sessions."))
	}

	if len(m.sessionList) > 0 {
		b.WriteString("\n")
		b.WriteString(ModalDimStyle.Render("↑↓/jk navigate  pgup/pgdn  enter resume  d delete  esc close"))
	} else {
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(ModalDimStyle.Render("Press esc to close"))

	return ModalBorderStyle.Render(b.String())
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

// --- Models Modal ---

func (m *Model) renderModelsModal() string {
	var b strings.Builder
	b.WriteString(ModalTitleStyle.Render("Available Models"))
	b.WriteString("\n\n")

	if len(m.modelList) == 0 {
		b.WriteString(ModalDimStyle.Render("No models available."))
		b.WriteString("\n\n")
		b.WriteString(ModalDimStyle.Render("Press esc to close"))
		return ModalBorderStyle.Render(b.String())
	}

	for i, mdl := range m.modelList {
		marker := " "
		if mdl.Current {
			marker = "*"
		}
		line := fmt.Sprintf("  %2d. %s", i+1, mdl.ID)
		if mdl.ContextLimit > 0 {
			line += fmt.Sprintf("  (n_ctx=%d)", mdl.ContextLimit)
		}
		line += " " + marker
		b.WriteString(ModalDimStyle.Render(line))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(ModalDimStyle.Render("Use /models <number> or /models <name> to switch."))
	b.WriteString("\n")
	b.WriteString(ModalDimStyle.Render("Press esc to close"))

	return ModalBorderStyle.Render(b.String())
}

func (m *Model) handleModelsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" || msg.String() == "q" {
		m.modal = ModalNone
		return m, nil
	}
	return m, nil
}

// --- Help Modal ---

func (m *Model) renderHelpModal() string {
	var b strings.Builder
	b.WriteString(ModalTitleStyle.Render("Keybindings"))
	b.WriteString("\n\n")

	sections := []struct {
		title string
		binds [][]string
	}{
		{"Commands", [][]string{
			{"enter", "Submit input"},
			{"ctrl+c", "Cancel turn / Quit"},
			{"ctrl+\\", "Force quit"},
			{"F1", "Show this help"},
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
			{"tab", "Complete (/resume)"},
		}},
		{"Viewport (esc to focus)", [][]string{
			{"↑ ↓ j k", "Scroll line"},
			{"pgup / pgdn", "Scroll page"},
			{"home / end", "Top / bottom"},
			{"mouse wheel", "Scroll viewport"},
			{"i / enter", "Return to input"},
		}},
		{"Slash Commands", [][]string{
			{"/plan / /act", "Toggle plan/act mode"},
			{"/models", "List/switch models"},
			{"/context", "Context usage details"},
			{"/new", "Start new session"},
			{"/resume", "List/restore/delete sessions"},
			{"/compact", "Compact history"},
			{"/verbose", "Toggle verbose output"},
			{"dir <path>", "Change working dir"},
			{"exit", "Quit GoGen"},
		}},
		{"Text Selection", [][]string{
			{"click+drag", "Select text in viewport"},
			{"ctrl+shift+c", "Copy selection"},
			{"right click", "Cancel selection"},
			{"esc", "Dismiss modal / focus viewport"},
		}},
	}

	for _, sec := range sections {
		b.WriteString(ModalDimStyle.Render(sec.title))
		b.WriteString("\n")
		for _, bind := range sec.binds {
			key := HelpKeyStyle.Render(bind[0])
			desc := HelpDescStyle.Render(bind[1])
			b.WriteString(fmt.Sprintf("  %-24s %s\n", key, desc))
		}
		b.WriteString("\n")
	}

	b.WriteString(ModalDimStyle.Render("any key to close"))

	return ModalBorderStyle.Render(b.String())
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
	var b strings.Builder
	for i, c := range m.completions {
		if i == m.completionIdx {
			b.WriteString(ModalHighlightStyle.Render(c))
		} else {
			b.WriteString(ModalDimStyle.Render(c))
		}
		if i < len(m.completions)-1 {
			b.WriteString("  ")
		}
	}
	return ModalBorderStyle.Render(b.String())
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
