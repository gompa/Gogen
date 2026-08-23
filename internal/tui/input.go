package tui

import (
	"strings"

	"gogen/internal/agent"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleInputKey dispatches key events when the input textarea has focus.
func (m *Model) handleInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m2, cmd, ok := m.handleInputHelpKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleVerboseKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleCancelOrQuitKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleSubmitKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleHistoryUpKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleHistoryDownKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleInputCompletionKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleDeleteForwardKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleEscapeKey(msg); ok {
		return m2, cmd
	}
	// Pass all other keys to textarea (it handles editing, word nav, kill, etc.)
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// handleInputHelpKey opens the help modal on F1 in any mode (but NOT "?" — that
// is a printable character).
func (m *Model) handleInputHelpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// F1 opens help in any mode (but NOT ? — that's a printable character)
	if key.Matches(msg, m.keys.Help) {
		m.modal = ModalHelp
		return m, nil, true
	}
	return m, nil, false
}

func (m *Model) handleVerboseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Global: verbose toggle
	if key.Matches(msg, m.keys.Verbose) {
		m.verbose = !m.verbose
		return m, nil, true
	}
	return m, nil, false
}

func (m *Model) handleCancelOrQuitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Cancel turn (ctrl+c) or quit
	if key.Matches(msg, m.keys.CancelTurn) {
		if m.streaming {
			m.streaming = false
			m.clearProgress()
			// Cancel the underlying LLM stream
			m.cancelActiveStream()
			m.resetStreamState(false)
			m.appendChatLine(SystemStyle.Render("Cancelled."))
			return m, m.refocusInput(), true
		}
		m.flushAndQuit()
		return m, tea.Quit, true
	}
	return m, nil, false
}

func (m *Model) handleSubmitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Submit (enter)
	if key.Matches(msg, m.keys.Submit) {
		if m.streaming {
			return m, nil, true
		}
		input := strings.TrimRight(m.textarea.Value(), "\n")
		if strings.TrimSpace(input) == "" {
			return m, nil, true
		}

		// Add to history
		if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != input {
			m.inputHistory = append(m.inputHistory, input)
		}
		m.historyIdx = len(m.inputHistory)

		// Check if it's a command
		if handled, quit, cmd := m.dispatchCommand(input); handled {
			m.textarea.Reset()
			if quit {
				return m, cmd, true
			}
			return m, tea.Batch(cmd, m.textarea.Focus()), true
		}

		// Send to agent
		cmd := m.submitUserInput(input)
		m.textarea.Reset()
		return m, tea.Batch(cmd, m.textarea.Focus()), true
	}
	return m, nil, false
}

func (m *Model) handleHistoryUpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// History navigation
	if key.Matches(msg, m.keys.HistoryUp) {
		if len(m.inputHistory) == 0 {
			return m, nil, true
		}
		if m.historyIdx == len(m.inputHistory) {
			m.historyDraft = m.textarea.Value()
		}
		if m.historyIdx > 0 {
			m.historyIdx--
			m.textarea.Reset()
			m.textarea.SetValue(m.inputHistory[m.historyIdx])
			m.textarea.CursorEnd()
		}
		return m, m.textarea.Focus(), true
	}
	return m, nil, false
}

func (m *Model) handleHistoryDownKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if key.Matches(msg, m.keys.HistoryDown) {
		if m.historyIdx >= len(m.inputHistory) {
			return m, nil, true
		}
		m.historyIdx++
		if m.historyIdx == len(m.inputHistory) {
			m.textarea.Reset()
			m.textarea.SetValue(m.historyDraft)
		} else {
			m.textarea.Reset()
			m.textarea.SetValue(m.inputHistory[m.historyIdx])
		}
		m.textarea.CursorEnd()
		return m, m.textarea.Focus(), true
	}
	return m, nil, false
}

func (m *Model) handleInputCompletionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Tab completion
	if key.Matches(msg, m.keys.Completion) {
		line := m.textarea.Value()
		if prefix, arg, ok := agent.ResumeLinePrefix(line); ok {
			completions := m.agent.ResumeArgCompletions(arg)
			if len(completions) == 0 {
				return m, nil, true
			}
			if len(completions) == 1 {
				newArg := completions[0]
				if newArg == "del" {
					newArg = "del "
				}
				m.textarea.Reset()
				m.textarea.SetValue(prefix + newArg)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			cp := agent.LongestCommonPrefix(completions)
			if len(cp) > len(arg) {
				m.textarea.Reset()
				m.textarea.SetValue(prefix + cp)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			// Show completion modal
			m.completions = completions
			m.completionIdx = 0
			m.completionLine = line
			m.modal = ModalCompletion
			return m, nil, true
		}
		if completions := agent.SlashCommandCompletions(line, false, true); len(completions) > 0 {
			if len(completions) == 1 {
				m.textarea.Reset()
				m.textarea.SetValue(completions[0] + " ")
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			cp := agent.LongestCommonPrefix(completions)
			trimmed := strings.TrimRight(line, " \t")
			if len(cp) > len(trimmed) {
				m.textarea.Reset()
				m.textarea.SetValue(cp)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			m.completions = completions
			m.completionIdx = 0
			m.completionLine = line
			m.modal = ModalCompletion
			return m, nil, true
		}
		return m, nil, true
	}
	return m, nil, false
}

func (m *Model) handleDeleteForwardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Ctrl+D on empty line = quit
	if key.Matches(msg, m.keys.DeleteForward) {
		val := strings.TrimSpace(m.textarea.Value())
		if val == "" {
			m.cancelActiveStream()
			m.dismissApproval(false)
			m.flushAndQuit()
			return m, tea.Quit, true
		}
	}
	return m, nil, false
}

func (m *Model) handleEscapeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Escape to focus viewport
	if msg.String() == "esc" {
		m.focus = FocusViewport
		m.clearSelection()
		m.textarea.Blur()
		return m, nil, true
	}
	return m, nil, false
}

// handleViewportKey dispatches key events when the viewport has focus.
func (m *Model) handleViewportKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Help: F1 or ?
	if key.Matches(msg, m.keys.Help) || msg.String() == "?" {
		m.modal = ModalHelp
		return m, nil
	}

	// Cancel turn or quit
	if key.Matches(msg, m.keys.CancelTurn) {
		if m.streaming {
			m.streaming = false
			m.clearProgress()
			// Cancel the underlying LLM stream
			m.cancelActiveStream()
			m.resetStreamState(false)
			m.appendChatLine(SystemStyle.Render("Cancelled."))
			// Stay on viewport focus; blink restarts when returning to input.
			return m, nil
		}
		m.flushAndQuit()
		return m, tea.Quit
	}

	// Focus back to input
	switch msg.String() {
	case "i", "enter":
		m.clearSelection()
		m.focus = FocusInput
		return m, m.textarea.Focus()
	case "esc":
		m.clearSelection()
		m.focus = FocusInput
		return m, m.textarea.Focus()
	case "tab":
		// Keyboard access to the sessions panel (web parity is mouse;
		// tab is the keyboard entry point). Tab is unused in viewport
		// focus — in input focus it stays completion.
		m.focus = FocusSidebar
		m.sidebarCursor = m.sidebarFocusedRow()
		return m, nil
	}

	// Any printable character switches to input and passes the character through.
	// v2: check the key code with no modifiers (v1's Runes field is gone;
	// ctrl/alt-modified letters carry the same Code but must not type text).
	if c := msg.Code; c >= 32 && c < 127 && msg.Mod == 0 {
		m.focus = FocusInput
		focusCmd := m.textarea.Focus()
		var updateCmd tea.Cmd
		m.textarea, updateCmd = m.textarea.Update(msg)
		return m, tea.Batch(focusCmd, updateCmd)
	}

	// Viewport scrolling
	switch msg.String() {
	case "up", "k":
		m.viewport.LineUp(1)
		return m, nil
	case "down", "j":
		m.viewport.LineDown(1)
		return m, nil
	case "pgup":
		m.viewport.PageUp()
		return m, nil
	case "pgdown", "ctrl+f":
		m.viewport.PageDown()
		return m, nil
	case "home", "g":
		m.viewport.GotoTop()
		return m, nil
	case "end", "G":
		m.viewport.GotoBottom()
		return m, nil
	}

	// Ctrl+U half page up (in viewport mode)
	if key.Matches(msg, m.keys.ViewportHalfUp) {
		m.viewport.HalfPageUp()
		return m, nil
	}
	// Ctrl+D half page down (in viewport mode)
	if key.Matches(msg, m.keys.ViewportHalfDown) {
		m.viewport.HalfPageDown()
		return m, nil
	}

	return m, nil
}
