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
	for _, cmd := range tuiCommands {
		if cmd.match(input, trimmed) {
			return cmd.run(m, input, trimmed)
		}
	}
	return false, false, nil
}

// tuiCommand is one slash-command family handled by dispatchCommand.
// Matchers are evaluated in table order (the original if/else order).
type tuiCommand struct {
	// match is a PURE string check (no side effects). The agent-delegated
	// families must accept exactly the inputs their agent handler accepts —
	// the handler runs only inside run — so commands cannot silently change
	// from handled to prompt-sent (or vice versa).
	match func(input, trimmed string) bool
	run   func(m *Model, input, trimmed string) (bool, bool, tea.Cmd)
}

// exactAny matches any of the given exact trimmed forms.
func exactAny(forms ...string) func(input, trimmed string) bool {
	return func(input, trimmed string) bool {
		for _, f := range forms {
			if trimmed == f {
				return true
			}
		}
		return false
	}
}

var tuiCommands = []tuiCommand{
	{match: exactAny("help", "/help"), run: cmdHelp},
	{match: exactAny("exit", "/exit", "quit", "/quit"), run: cmdExit},
	{match: exactAny("compact", "/compact"), run: cmdCompact},
	{match: exactAny("verbose", "/verbose"), run: cmdVerbose},
	{match: exactAny("subagents", "/subagents"), run: cmdSubagents},
	// Mode commands: mirror HandleModeCommand's accepted forms.
	{match: exactAny("plan", "/plan", "act", "/act", "mode", "/mode"), run: cmdMode},
	// Thinking command: mirror HandleThinkingCommand's accepted forms.
	{match: func(input, trimmed string) bool {
		return trimmed == "think" || trimmed == "/think" ||
			strings.HasPrefix(trimmed, "think ") || strings.HasPrefix(trimmed, "/think ")
	}, run: cmdThinking},
	{match: exactAny("context", "/context"), run: cmdContext},
	// Session commands: reuse the agent's pure parser, mirroring its cases.
	{match: func(input, trimmed string) bool {
		cmd, _ := agent.ParseSessionCommand(input)
		switch cmd {
		case "new", "sessions", "resume", "fork":
			return true
		}
		return false
	}, run: cmdSession},
	// save-config matches the RAW input (trailing whitespace matters),
	// preserving the original exact-string check.
	{match: func(input, trimmed string) bool {
		return input == "/save-config" || input == "save-config"
	}, run: cmdSaveConfig},
	// Models command: reuse the agent's pure parser.
	{match: func(input, trimmed string) bool {
		_, ok := agent.ParseModelsCommand(input)
		return ok
	}, run: cmdModels},
	{match: func(input, trimmed string) bool { return strings.HasPrefix(trimmed, "dir ") }, run: cmdDir},
}

// cmdSubagents opens the nested-session (subagent) list modal.
func cmdSubagents(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.modal = ModalSubagents
	return true, false, nil
}

func cmdHelp(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.modal = ModalHelp
	return true, false, nil
}

func cmdExit(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.flushAndQuit()
	return true, true, tea.Quit
}

func cmdCompact(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	if err := m.agent.CompactHistory(m.ctx); err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Compact failed: %v", err)))
	} else {
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("History compacted (%d messages remaining).", len(m.agent.Messages))))
	}
	return true, false, nil
}

func cmdVerbose(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.verbose = !m.verbose
	state := "off"
	if m.verbose {
		state = "on"
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Verbose tool output: %s", state)))
	return true, false, nil
}

func cmdMode(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _ := m.agent.HandleModeCommand(input)
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

func cmdThinking(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _ := m.agent.HandleThinkingCommand(input)
	m.appendChatLine(SystemStyle.Render(out))
	m.checkPersistError()
	return true, false, nil
}

func cmdContext(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _ := m.agent.HandleContextCommand(m.ctx, input)
	m.appendChatLine(SystemStyle.Render(out))
	m.checkPersistError()
	return true, false, nil
}

func cmdSession(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	result, _, err := m.agent.HandleSessionCommand(m.ctx, input, session.NewID())
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

func cmdSaveConfig(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	if err := m.saveConfig(false); err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Save config failed: %v", err)))
	}
	return true, false, nil
}

func cmdModels(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _, err := m.agent.HandleModelsCommand(m.ctx, input)
	// If no selector, show interactive modal
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/models"), "models"))
	if arg == "" {
		list, listErr := m.agent.ListModels(m.ctx)
		if listErr != nil {
			m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Models: %v", listErr)))
		} else {
			m.modelList = list
			m.modelCursor = 0
			m.modal = ModalModels
		}
		return true, false, nil
	}
	// Has selector — do inline switch
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Models: %v", err)))
	} else {
		m.appendChatLine(SystemStyle.Render(out))
	}
	return true, false, nil
}

func cmdDir(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	newDir := strings.TrimSpace(strings.TrimPrefix(trimmed, "dir "))
	absDir, err := filepath.Abs(newDir)
	if err != nil || !agent.DirExists(absDir) {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Error: directory does not exist: %s", newDir)))
	} else {
		m.agent.SetWorkingDir(absDir)
		m.agent.AfterWorkingDirChange()
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Changed working directory to: %s", absDir)))
	}
	m.checkPersistError()
	return true, false, nil
}

func (m *Model) saveConfig(includeSecrets bool) error {
	cfgPath, guidelinesPath, err := m.agent.SaveConfig(m.cfg, includeSecrets)
	if err != nil {
		return err
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Wrote config to %s", cfgPath)))
	if guidelinesPath != "" {
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Wrote guidelines to %s", guidelinesPath)))
	}
	m.appendChatLine(SystemStyle.Render("Note: environment variables still override file values at runtime."))
	return nil
}

// deliveryRequestMsg requests a system message delivery (job completion
// notice, scheduled reminder, subagent report). Producers send it via
// m.program.Send from arbitrary goroutines; Update appends it to
// pendingDeliveries and drains when the TUI is idle, so Messages is only
// ever touched on the Update thread (via the streamCmd goroutine, the same
// ownership contract as user input).
type deliveryRequestMsg struct {
	text string
}

// submitDeliveredText renders a system-delivered message as a user line and
// runs a turn on it — the same flow as submitUserInput minus the input
// history bookkeeping. Callers must ensure !m.streaming.
func (m *Model) submitDeliveredText(text string) tea.Cmd {
	m.appendChatLine(SystemStyle.Render(noticeLabel + " " + text))
	m.streaming = true
	m.resetStreamState(false)
	startProgress := m.setProgress(progressThinking, "thinking")

	streamCtx, cancelFn := context.WithCancel(m.ctx)
	m.streamCancel = cancelFn

	adapter := NewStreamAdapter(m.program)
	a := m.agent
	approver := m.makeDeleteApprover()

	streamCmd := func() tea.Msg {
		defer cancelFn()
		_, err := a.StreamProcessInput(
			agent.ContextWithDeleteApprover(streamCtx, approver),
			text,
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

// drainDeliveries starts the next queued system delivery when the TUI is
// idle. Returns nil when nothing is queued or a turn is running; the next
// delivery (if any) drains at the next stream end.
func (m *Model) drainDeliveries() tea.Cmd {
	if m.streaming {
		return nil
	}
	if m.deliveryDrops > 0 {
		// Report overflow drops only at a drain boundary (never mid-stream,
		// where an appended line would interleave with the stream's line
		// bookkeeping).
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("%d background message(s) dropped (delivery queue full).", m.deliveryDrops)))
		m.deliveryDrops = 0
	}
	if len(m.pendingDeliveries) == 0 {
		return nil
	}
	text := m.pendingDeliveries[0]
	m.pendingDeliveries = m.pendingDeliveries[1:]
	return m.submitDeliveredText(text)
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
