package tui

import (
	"fmt"
	"strings"

	"gogen/internal/agent"

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
		// Approve
		m.approvalResult <- true
		m.approvalUI = nil
		m.modal = ModalNone
		return m, nil
	case "n", "esc":
		// Reject
		m.approvalResult <- false
		m.approvalUI = nil
		m.modal = ModalNone
		return m, nil
	}
	return m, nil
}

// --- Sessions Modal ---

func (m *Model) renderSessionsModal() string {
	var b strings.Builder
	b.WriteString(ModalTitleStyle.Render("Saved Sessions"))
	b.WriteString("\n\n")

	if len(m.sessionList) == 0 {
		b.WriteString(ModalDimStyle.Render("No saved sessions."))
		b.WriteString("\n\n")
		b.WriteString(ModalDimStyle.Render("Press esc to close"))
		return ModalBorderStyle.Render(b.String())
	}

	for _, s := range m.sessionList {
		line := fmt.Sprintf("  %s  (%d msgs)", s.ID, s.MessageCount)
		if s.Label != "" {
			line += fmt.Sprintf("  \"%s\"", s.Label)
		}
		if s.ID == m.sessionID {
			line += "  ← current"
		}
		b.WriteString(ModalDimStyle.Render(line))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(ModalDimStyle.Render("resume <id>  |  resume latest  |  resume del <id>"))
	b.WriteString("\n")
	b.WriteString(ModalDimStyle.Render("Press esc to close"))

	return ModalBorderStyle.Render(b.String())
}

func (m *Model) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" || msg.String() == "q" {
		m.modal = ModalNone
		return m, nil
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
