package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/session"

	tea "github.com/charmbracelet/bubbletea"
)

// dispatchCommand handles slash commands and other special inputs.
// Returns (handled, quit, tea.Cmd).
func (m *Model) dispatchCommand(input string) (bool, bool, tea.Cmd) {
	trimmed := strings.TrimSpace(input)

	// exit
	if trimmed == "exit" || trimmed == "/exit" || trimmed == "quit" || trimmed == "/quit" {
		// Session saving happens automatically via the agent
		return true, true, tea.Quit
	}

	// compact
	if trimmed == "compact" || trimmed == "/compact" {
		if err := m.agent.CompactHistory(m.ctx); err != nil {
			m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Compact failed: %v", err)))
		} else {
			m.appendChatLine(SystemStyle.Render(fmt.Sprintf("History compacted (%d messages remaining).", len(m.agent.Messages))))
		}
		return true, false, nil
	}

	// verbose toggle
	if trimmed == "verbose" || trimmed == "/verbose" {
		m.verbose = !m.verbose
		state := "off"
		if m.verbose {
			state = "on"
		}
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Verbose tool output: %s", state)))
		return true, false, nil
	}

	// Mode commands
	if out, handled := m.agent.HandleModeCommand(input); handled {
		if trimmed == "/plan" || trimmed == "plan" || trimmed == "/act" || trimmed == "act" {
			m.chatLines = renderMessages(m.agent.Messages, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())
			m.chatLines = append(m.chatLines, SystemStyle.Render(out))
			m.setViewportContent()
			m.viewport.GotoBottom()
		} else {
			m.appendChatLine(SystemStyle.Render(out))
		}
		// SetMode persists the session.
		m.checkPersistError()
		return true, false, nil
	}

	// Context command
	if out, handled := m.agent.HandleContextCommand(m.ctx, input); handled {
		m.appendChatLine(SystemStyle.Render(out))
		m.checkPersistError()
		return true, false, nil
	}

	// Session commands
	if result, handled, err := m.agent.HandleSessionCommand(m.ctx, input, session.NewID()); handled {
		if err != nil {
			m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
			// Errors from startNewSession/deleteSessionByID may surface a
			// half-completed persist; check anyway.
			m.checkPersistError()
			return true, false, nil
		}
		if result.Action == agent.SessionActionClearChat {
			// Clear chat and show new session info
			m.chatLines = nil
			m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
			if len(result.History) > 0 {
				m.chatLines = append(m.chatLines, renderMessages(result.History, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())...)
			}
			m.setViewportContent()
			m.viewport.GotoBottom()
			m.sessionID = m.agent.SessionID
			m.refreshContextStats()
			return true, false, nil
		} else if result.Sessions != nil {
			// Show session list modal
			m.sessionList = result.Sessions
			m.sessionCursor = 0
			m.modal = ModalSessions
		} else {
			m.appendChatLine(SystemStyle.Render(result.Output))
			if len(result.History) > 0 {
				m.chatLines = renderMessages(result.History, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())
				m.setViewportContent()
				m.viewport.GotoBottom()
			}
		}
		// Session commands (new, resume, delete) persist.
		m.checkPersistError()
		return true, false, nil
	}

	// save-config
	if input == "/save-config" || input == "save-config" {
		if err := m.saveConfig(false); err != nil {
			m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Save config failed: %v", err)))
		}
		return true, false, nil
	}

	// Models command
	if out, handled, err := m.agent.HandleModelsCommand(m.ctx, input); handled {
		if err != nil {
			m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Models: %v", err)))
		} else {
			m.appendChatLine(SystemStyle.Render(out))
		}
		return true, false, nil
	}

	// dir command
	if strings.HasPrefix(trimmed, "dir ") {
		newDir := strings.TrimSpace(strings.TrimPrefix(trimmed, "dir "))
		absDir, err := filepath.Abs(newDir)
		if err != nil || !agent.DirExists(absDir) {
			m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Error: directory does not exist: %s", newDir)))
		} else {
			m.agent.SetWorkingDir(absDir)
			m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Changed working directory to: %s", absDir)))
		}
		// SetWorkingDir persists the session.
		m.checkPersistError()
		return true, false, nil
	}

	return false, false, nil
}

func (m *Model) saveConfig(includeSecrets bool) error {
	cfgPath, guidelinesPath, err := m.agent.SaveConfig(m.cfg, includeSecrets)
	if err != nil {
		return err
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Wrote config to %s", cfgPath)))
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Wrote guidelines to %s", guidelinesPath)))
	m.appendChatLine(SystemStyle.Render("Note: environment variables still override file values at runtime."))
	return nil
}

// submitUserInput sends user input to the agent for processing.
func (m *Model) submitUserInput(input string) tea.Cmd {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil
	}

	// Update history
	if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != trimmed {
		m.inputHistory = append(m.inputHistory, trimmed)
	}
	m.historyIdx = len(m.inputHistory)

	// Show user message in chat
	m.appendChatLine(UserStyle.Render(userLabel) + " " + trimmed)

	// Start streaming
	m.streaming = true
	m.resetStreamState(false)
	startProgress := m.setProgress(progressThinking, "thinking")

	// Create cancelable context for the LLM call
	streamCtx, cancelFn := context.WithCancel(m.ctx)
	m.streamCancel = cancelFn

	adapter := NewStreamAdapter(m.program)
	a := m.agent
	approver := m.makeDeleteApprover()

	streamCmd := func() tea.Msg {
		defer cancelFn()
		_, err := a.StreamProcessInput(
			agent.ContextWithDeleteApprover(streamCtx, approver),
			trimmed,
			adapter.Handlers(),
		)
		if err != nil {
			return streamErrorMsg{err: err}
		}
		// Return streamEndMsg directly so handleStreamEnd refreshes context
		// stats synchronously after Messages are final.
		return streamEndMsg{}
	}
	return tea.Batch(startProgress, streamCmd)
}

// makeDeleteApprover creates a delete approver that shows a modal.
func (m *Model) makeDeleteApprover() agent.DeleteApprover {
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Mark this approval as in-flight BEFORE draining/sending so a stale
		// dismissal from a previous cancelled approval cannot masquerade as
		// this approval's response.
		m.approvalInFlight = true
		defer func() { m.approvalInFlight = false }()
		// Drain stale value from previous approval (e.g. context cancelled
		// while user still responded to modal). The flag above guarantees
		// any value sitting in the channel belongs to the previous approver.
		select {
		case <-m.approvalResult:
		default:
		}
		// Show approval modal via Bubble Tea
		m.program.Send(approvalRequestMsg{req: req})
		// Wait for result from the channel
		select {
		case approved := <-m.approvalResult:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// approvalRequestMsg is an internal message to trigger the approval modal.
type approvalRequestMsg struct {
	req agent.DeleteRequest
}
