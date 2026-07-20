package tui

import (
	"strings"

	"gogen/internal/agent"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// handleInputKey dispatches key events when the input textarea has focus.
func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// F1 opens help in any mode (but NOT ? — that's a printable character)
	if key.Matches(msg, m.keys.Help) {
		m.modal = ModalHelp
		return m, nil
	}

	// Global: verbose toggle
	if key.Matches(msg, m.keys.Verbose) {
		m.verbose = !m.verbose
		return m, nil
	}

	// Cancel turn (ctrl+c) or quit
	if key.Matches(msg, m.keys.CancelTurn) {
		if m.streaming {
			m.streaming = false
			m.clearProgress()
			// Cancel the underlying LLM stream
			if m.streamCancel != nil {
				m.streamCancel()
			}
			m.resetStreamState(false)
			m.appendChatLine(SystemStyle.Render("Cancelled."))
			return m, m.refocusInput()
		}
		m.quitting = true
		return m, tea.Quit
	}

	// Submit (enter)
	if key.Matches(msg, m.keys.Submit) {
		if m.streaming {
			return m, nil
		}
		input := strings.TrimRight(m.textarea.Value(), "\n")
		if strings.TrimSpace(input) == "" {
			return m, nil
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
				return m, cmd
			}
			return m, tea.Batch(cmd, m.textarea.Focus())
		}

		// Send to agent
		cmd := m.submitUserInput(input)
		m.textarea.Reset()
		return m, tea.Batch(cmd, m.textarea.Focus())
	}

	// History navigation
	if key.Matches(msg, m.keys.HistoryUp) {
		if len(m.inputHistory) == 0 {
			return m, nil
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
		return m, m.textarea.Focus()
	}

	if key.Matches(msg, m.keys.HistoryDown) {
		if m.historyIdx >= len(m.inputHistory) {
			return m, nil
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
		return m, m.textarea.Focus()
	}

	// Tab completion
	if key.Matches(msg, m.keys.Completion) {
		line := m.textarea.Value()
		if prefix, arg, ok := agent.ResumeLinePrefix(line); ok {
			completions := m.agent.ResumeArgCompletions(arg)
			if len(completions) == 0 {
				return m, nil
			}
			if len(completions) == 1 {
				newArg := completions[0]
				if newArg == "del" {
					newArg = "del "
				}
				m.textarea.Reset()
				m.textarea.SetValue(prefix + newArg)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus()
			}
			cp := agent.LongestCommonPrefix(completions)
			if len(cp) > len(arg) {
				m.textarea.Reset()
				m.textarea.SetValue(prefix + cp)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus()
			}
			// Show completion modal
			m.completions = completions
			m.completionIdx = 0
			m.completionLine = line
			m.modal = ModalCompletion
			return m, nil
		}
		if completions := agent.SlashCommandCompletions(line, false, true); len(completions) > 0 {
			if len(completions) == 1 {
				m.textarea.Reset()
				m.textarea.SetValue(completions[0] + " ")
				m.textarea.CursorEnd()
				return m, m.textarea.Focus()
			}
			cp := agent.LongestCommonPrefix(completions)
			trimmed := strings.TrimRight(line, " \t")
			if len(cp) > len(trimmed) {
				m.textarea.Reset()
				m.textarea.SetValue(cp)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus()
			}
			m.completions = completions
			m.completionIdx = 0
			m.completionLine = line
			m.modal = ModalCompletion
			return m, nil
		}
		return m, nil
	}

	// Ctrl+D on empty line = quit
	if key.Matches(msg, m.keys.DeleteForward) {
		val := strings.TrimSpace(m.textarea.Value())
		if val == "" {
			if m.streamCancel != nil {
				m.streamCancel()
			}
			m.dismissApproval(false)
			m.quitting = true
			return m, tea.Quit
		}
	}

	// Escape to focus viewport
	if msg.String() == "esc" {
		m.focus = FocusViewport
		m.clearSelection()
		m.textarea.Blur()
		return m, nil
	}

	// Pass all other keys to textarea (it handles editing, word nav, kill, etc.)
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// handleViewportKey dispatches key events when the viewport has focus.
func (m *Model) handleViewportKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
			if m.streamCancel != nil {
				m.streamCancel()
			}
			m.resetStreamState(false)
			m.appendChatLine(SystemStyle.Render("Cancelled."))
			// Stay on viewport focus; blink restarts when returning to input.
			return m, nil
		}
		m.quitting = true
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
	}

	// Any printable character switches to input and passes the character through
	if len(msg.Runes) == 1 {
		r := msg.Runes[0]
		if r >= 32 && r < 127 {
			m.focus = FocusInput
			focusCmd := m.textarea.Focus()
			var updateCmd tea.Cmd
			m.textarea, updateCmd = m.textarea.Update(msg)
			return m, tea.Batch(focusCmd, updateCmd)
		}
	}

	// Viewport scrolling
	switch msg.String() {
	case "up", "k":
		m.viewport.LineUp(1)
		return m, nil
	case "down", "j":
		m.viewport.LineDown(1)
		return m, nil
	case "pgup", "ctrl+b":
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

