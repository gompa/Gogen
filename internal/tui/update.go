package tui

import (
	"gogen/internal/agent"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbletea/v2"
)

func (m *Model) Init() tea.Cmd {
	// The sidebar tick loop runs for the whole program lifetime (a no-op
	// while the panel is hidden) so toggling can never fork a second loop.
	return tea.Batch(m.textarea.Focus(), sidebarTickCmd())
}

// Update implements the Bubble Tea update contract; the per-message dispatch
// lives in handleMsg. On bubbletea v2 the terminal's key-disambiguation mode
// (kitty protocol / xterm modifyOtherKeys) is negotiated automatically, so
// ctrl+shift+c arrives as a normal KeyPressMsg and needs no special-casing.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return m.handleMsg(msg)
}

// handleMsg dispatches a tea message to its per-type handler. The textarea
// fallback at the end (cursor blink + normal input) runs for any message no
// case consumed — including the per-type handlers' own sub-messages.
func (m *Model) handleMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Drop streaming render events whose owner lost focus between send and
	// delivery (switch-boundary stragglers). Terminal msgs carry sid too
	// and are routed by handleTurnFinishedMsg instead.
	if sid, ok := streamEventSid(msg); ok && !m.ownsStream(sid) {
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSizeMsg(msg)

	// Terminal window focus (View's ReportFocus): the completion bell
	// rings only while the window is blurred.
	case tea.FocusMsg:
		m.terminalBlurred = false
		return m, nil

	case tea.BlurMsg:
		m.terminalBlurred = true
		return m, nil

	case modelChangedMsg:
		return m.handleModelChangedMsg()

	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)

	case tea.KeyPressMsg:
		// Only key presses are dispatched; key releases are ignored so a
		// terminal reporting event types cannot double-trigger actions.
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
		return m.handleTurnFinishedMsg(msg.sid, nil)

	case streamErrorMsg:
		return m.handleTurnFinishedMsg(msg.sid, msg.err)

	case condensedNoteMsg:
		return m.handleCondensedNoteMsg(msg)

	case contextStatsMsg:
		return m.handleContextStatsMsg(msg)

	case sidebarTickMsg:
		// 30 s relative-time / store refresh (web's sidebar tick).
		if m.sidebarVisible {
			m.refreshSavedSessions()
		}
		return m, sidebarTickCmd()

	// Approval request (show modal)
	case approvalRequestMsg:
		return m.handleApprovalRequestMsg(msg)

	// System message delivery (queue + drain when idle)
	case deliveryRequestMsg:
		if len(m.pendingDeliveries) >= maxPendingDeliveries {
			// Overflow: drop the OLDEST queued delivery (freshness wins —
			// a stale job notice is worse than none), mirroring the web
			// delivery queue. The drop is reported at the next drain.
			m.pendingDeliveries = m.pendingDeliveries[1:]
			m.deliveryDrops++
		}
		m.pendingDeliveries = append(m.pendingDeliveries, msg.text)
		return m, m.drainDeliveries()

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

func (m *Model) handleKeyMsg(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Clear transient status message on any key press
	if m.statusMsg != "" {
		m.statusMsg = ""
	}
	if key.Matches(msg, m.keys.CopySelection) {
		_, cmd := m.copySelection()
		return m, cmd
	}
	// Global hotkeys that work regardless of focus/modal
	switch {
	case key.Matches(msg, m.keys.ForceQuit):
		m.cancelActiveStream()
		m.dismissApproval(false)
		m.flushAndQuit()
		return m, tea.Quit
	case msg.String() == "ctrl+b" && m.modal == ModalNone:
		m.toggleSidebar()
		return m, nil
	case (m.focus == FocusViewport || m.focus == FocusSidebar) && m.modal == ModalNone && msg.String() == "[":
		m.resizeSidebar(-4)
		return m, nil
	case (m.focus == FocusViewport || m.focus == FocusSidebar) && m.modal == ModalNone && msg.String() == "]":
		m.resizeSidebar(4)
		return m, nil
	}

	// If a modal is active, handle modal keys
	if m.modal != ModalNone {
		return m.handleModalKey(msg)
	}

	// Dispatch based on focus
	if m.focus == FocusViewport {
		return m.handleViewportKey(msg)
	}
	if m.focus == FocusSidebar {
		// Auto-hide (narrow terminal) can leave focus on an invisible
		// panel — fall back to the input instead of swallowing keys.
		if m.sidebarOffsetX() == 0 {
			m.focus = FocusInput
			return m, m.textarea.Focus()
		}
		return m.handleSidebarKey(msg)
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
	m.bellIfBlurred() // web parity: "Agent finished responding" notification
	return m, tea.Batch(m.refocusInput(), m.drainDeliveries())
}

func (m *Model) handleCondensedNoteMsg(msg condensedNoteMsg) (tea.Model, tea.Cmd) {
	// Last-resort condensation announcement (Phase 0e): a system line in
	// the chat, not part of the LLM history.
	m.appendChatLine(DimStyle.Render("System: " + msg.note))
	return m, nil
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
	m.bellIfBlurred() // web parity: "Approval needed" notification
	return m, nil
}

func (m *Model) handleMouseMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Border drag-resize, panel clicks, and panel-area events take
	// precedence over text selection; wheel events over the chat fall
	// through to the viewport.
	if ev, ok := normalizeMouseEvent(msg); ok {
		if m.handleSidebarResizeMouse(ev) {
			return m, nil
		}
		if m.handleSidebarMouse(ev) {
			// The panel consumed the event: the mouse is over the panel
			// area, so its border renders highlighted.
			m.sidebarHovering = true
			return m, nil
		}
		m.sidebarHovering = false
		if m.handleMouseSelection(ev) {
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, tea.Batch(cmd)
}
