package tui

import (
	"gogen/internal/agent"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbletea"
)

func (m *Model) Init() tea.Cmd {
	return m.textarea.Focus()
}

// Update implements the Bubble Tea update contract. The per-message dispatch
// lives in handleMsg; this wrapper only handles the undecoded ctrl+shift+c
// sequence (kitty / xterm modifyOtherKeys) that never arrives as a KeyMsg.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Enhanced keyboard: ctrl+shift+c may arrive as an undecoded CSI sequence
	// (kitty / xterm modifyOtherKeys) rather than a KeyMsg.
	if _, ok := msg.(tea.KeyMsg); !ok && isCtrlShiftC(msg) {
		if m.statusMsg != "" {
			m.statusMsg = ""
		}
		m.copySelection()
		return m, nil
	}
	return m.handleMsg(msg)
}

// handleMsg dispatches a tea message to its per-type handler. The textarea
// fallback at the end (cursor blink + normal input) runs for any message no
// case consumed — including the per-type handlers' own sub-messages.
func (m *Model) handleMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSizeMsg(msg)

	case modelChangedMsg:
		return m.handleModelChangedMsg()

	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)

	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	// Streaming messages
	case streamStartMsg:
		return m.handleStreamStartMsg()

	case streamRoundStartMsg:
		return m.handleStreamRoundStartMsg()

	case streamTokenMsg:
		return m.handleStreamTokenMsg(msg)

	case streamThinkingMsg:
		return m.handleStreamThinkingMsg(msg)

	case streamToolCallMsg:
		return m.handleStreamToolCallMsg(msg)

	case streamToolCallArgsMsg:
		return m.handleStreamToolCallArgsMsg(msg)

	case streamToolCallFinalMsg:
		return m.handleStreamToolCallFinalMsg(msg)

	case streamToolExecuteMsg:
		return m.handleStreamToolExecuteMsg(msg)

	case streamToolResultMsg:
		return m.handleStreamToolResultMsg(msg)

	case streamRoundEndMsg:
		return m.handleStreamRoundEndMsg()

	case streamEndMsg:
		return m.handleStreamEndMsg()

	case streamErrorMsg:
		return m.handleStreamErrorMsg(msg)

	case contextStatsMsg:
		return m.handleContextStatsMsg(msg)

	// Approval request (show modal)
	case approvalRequestMsg:
		return m.handleApprovalRequestMsg(msg)

	// Pass mouse events to the viewport for wheel scrolling
	case tea.MouseMsg:
		return m.handleMouseMsg(msg)
	}

	// Update textarea for cursor blink and normal input
	if m.focus == FocusInput && m.modal == ModalNone && !m.streaming {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleWindowSizeMsg(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.SetSize(msg.Width, msg.Height)
	return m, nil
}

func (m *Model) handleModelChangedMsg() (tea.Model, tea.Cmd) {
	return m, nil
}

func (m *Model) handleSpinnerTick(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	if !m.progressAnimating() {
		return m, nil
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
}

func (m *Model) handleStreamStartMsg() (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamStart()
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamRoundStartMsg() (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamRoundStart()
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamTokenMsg(msg streamTokenMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamToken(msg.token)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamThinkingMsg(msg streamThinkingMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamThinking(msg.token)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamToolCallMsg(msg streamToolCallMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamToolCall(msg.index, msg.id, msg.name)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamToolCallArgsMsg(msg streamToolCallArgsMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamToolArgs(msg.index, msg.id, msg.delta)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamToolCallFinalMsg(msg streamToolCallFinalMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamToolCallFinal(msg.index, msg.tc)
	return m, nil
}

func (m *Model) handleStreamToolExecuteMsg(msg streamToolExecuteMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	label := "running tool"
	if msg.name != "" {
		label = "running " + msg.name
	}
	return m, m.setProgress(progressTool, label)
}

func (m *Model) handleStreamToolResultMsg(msg streamToolResultMsg) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamToolResult(msg.id, msg.name, msg.result, msg.success)
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamRoundEndMsg() (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.handleStreamRoundEnd()
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamEndMsg() (tea.Model, tea.Cmd) {
	// Always process – resets streaming state
	m.handleStreamEnd()
	return m, m.refocusInput()
}

func (m *Model) handleStreamErrorMsg(msg streamErrorMsg) (tea.Model, tea.Cmd) {
	m.handleStreamError(msg.err)
	return m, m.refocusInput()
}

func (m *Model) handleContextStatsMsg(msg contextStatsMsg) (tea.Model, tea.Cmd) {
	// Optional async refresh (session commands); stream end updates sync.
	m.contextStats = msg.stats
	m.contextLine = agent.FormatContextBrief(msg.stats)
	return m, nil
}

func (m *Model) handleApprovalRequestMsg(msg approvalRequestMsg) (tea.Model, tea.Cmd) {
	m.approvalUI = &approvalUIState{
		paths:  msg.req.Paths,
		reason: msg.req.Reason,
		cursor: 1, // default to Yes
	}
	m.modal = ModalApproval
	return m, nil
}

func (m *Model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Check for text selection first; wheel events fall through to viewport
	if m.handleMouseSelection(msg) {
		return m, nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, tea.Batch(cmd)
}
