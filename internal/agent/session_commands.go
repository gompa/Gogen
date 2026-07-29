package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gogen/internal/llm"
)

// SessionCommandAction tells clients about side effects (e.g. clear chat UI).
type SessionCommandAction string

const (
	SessionActionNone      SessionCommandAction = ""
	SessionActionClearChat SessionCommandAction = "clear_chat"
)

// SessionCommandResult is the outcome of a session slash command.
type SessionCommandResult struct {
	Output   string
	Action   SessionCommandAction
	Sessions []SessionInfo
	History  []llm.Message
}

// HandleSessionCommand processes /new, /resume, and sessions commands.
// newSessionID is required for /new (call session.NewID() from the caller).
func (a *Agent) HandleSessionCommand(ctx context.Context, input, newSessionID string) (SessionCommandResult, bool, error) {
	cmd, args := parseSessionCommand(input)
	switch cmd {
	case "new":
		out, err := a.startNewSession(newSessionID)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: AppendContextBrief(ctx, a, out), Action: SessionActionClearChat}, true, nil
	case "sessions", "resume":
		if args != "" {
			return a.handleResumeArg(ctx, args, newSessionID)
		}
		out, sessions, err := a.formatSessionList()
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Sessions: sessions}, true, nil
	case "fork":
		if err := a.ForkSession(ctx, args, newSessionID); err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: AppendContextBrief(ctx, a, fmt.Sprintf("Forked new session %s.", newSessionID)), Action: SessionActionClearChat, History: append([]llm.Message(nil), a.Messages...)}, true, nil
	}
	return SessionCommandResult{}, false, nil
}

// parseSessionCommand splits input into (command, args), stripping a leading "/".
func parseSessionCommand(input string) (cmd, args string) {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.TrimPrefix(trimmed, "/")
	parts := strings.SplitN(trimmed, " ", 2)
	cmd = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return cmd, args
}

// ResetSessionState clears all session-related state for starting fresh.
// This is used when creating a new session or replacing a deleted current session.
func (a *Agent) ResetSessionState() {
	a.Messages = nil
	a.clearTurnUsage()
	a.restoredTokenCounts = nil
	a.UsageAccum = UsageAccumulator{}
	a.resetSaveTracking()
	a.clearViewDriftSnapshot()
	a.SessionLabel = ""
	a.SessionOneshot = false
	if a.PinManager != nil {
		a.PinManager.ClearPins()
	}
	if a.TodoManager != nil {
		a.TodoManager.Clear()
	}
}

// handleResumeArg routes "del", "latest", or session-ID sub-commands.
func (a *Agent) handleResumeArg(ctx context.Context, args, newSessionID string) (SessionCommandResult, bool, error) {
	if args == "del" {
		return SessionCommandResult{}, true, fmt.Errorf("usage: resume del <id>")
	}
	if strings.HasPrefix(args, "del ") {
		delID := strings.TrimSpace(strings.TrimPrefix(args, "del"))
		if delID == "" {
			return SessionCommandResult{}, true, fmt.Errorf("usage: resume del <id>")
		}
		out, action, err := a.deleteSessionByID(ctx, delID, newSessionID)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Action: action}, true, nil
	}
	if args == "latest" {
		out, err := a.resumeLatestSession(ctx)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: append([]llm.Message(nil), a.Messages...)}, true, nil
	}
	out, err := a.resumeSessionByID(ctx, args)
	if err != nil {
		return SessionCommandResult{}, true, err
	}
	return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: append([]llm.Message(nil), a.Messages...)}, true, nil
}

func (a *Agent) startNewSession(newID string) (string, error) {
	if strings.TrimSpace(newID) == "" {
		return "", fmt.Errorf("session id is required")
	}
	oldID := a.SessionID
	if a.SessionStore != nil {
		a.FlushSession()
	}
	a.SessionID = newID
	a.ResetSessionState()
	if a.SessionStore != nil {
		a.FlushSession()
		if oldID != "" {
			return fmt.Sprintf("New session %s. Previous session %s saved — use `resume %s` to restore.",
				newID, oldID, oldID), nil
		}
		return fmt.Sprintf("New session %s.", newID), nil
	}
	return "New in-memory session (persistence disabled — history not saved).", nil
}

func (a *Agent) resumeSessionByID(ctx context.Context, id string) (string, error) {
	if a.SessionStore == nil {
		return "", fmt.Errorf("session persistence disabled")
	}
	// Flush the current session only if it has unsaved changes, so we don't
	// write a fresh timestamp on a session that was merely loaded.
	if a.sessionDirty {
		a.FlushSession()
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("session id is required")
	}
	snap, err := a.SessionStore.LoadInWorkingDir(a.WorkingDir, id)
	if err != nil {
		return "", err
	}
	// Restore locally so the client gets history immediately. Provider
	// validation / context-limit refresh runs in the background (same as
	// process startup) and must not block the resume WS response.
	model := snap.Model
	a.RestoreSessionLocal(snap, id)
	a.SessionID = id
	label := llm.SessionLabel(snap.Messages, llm.DefaultSessionLabelMaxLen)
	var out string
	if label != "" {
		out = fmt.Sprintf("Resumed session %s (%d messages): \"%s\"", id, len(snap.Messages), label)
	} else {
		out = fmt.Sprintf("Resumed session %s (%d messages).", id, len(snap.Messages))
	}
	// Build the context brief before background model validation so we don't
	// race Snapshot against RefreshAfterModelChange.
	out = AppendContextBrief(ctx, a, out)
	go a.ValidateRestoredModel(context.Background(), model)
	return out, nil
}

func (a *Agent) resumeLatestSession(ctx context.Context) (string, error) {
	if a.SessionStore == nil {
		return "", fmt.Errorf("session persistence disabled")
	}
	list, err := a.SessionStore.List(a.WorkingDir)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no saved sessions")
	}
	if len(list) == 1 && list[0].ID == a.SessionID {
		return "", fmt.Errorf("no other saved sessions to resume")
	}
	target := list[0].ID
	for _, s := range list {
		if s.ID != a.SessionID {
			target = s.ID
			break
		}
	}
	return a.resumeSessionByID(ctx, target)
}

func (a *Agent) deleteSessionByID(ctx context.Context, id, newSessionID string) (string, SessionCommandAction, error) {
	if a.SessionStore == nil {
		return "", SessionActionNone, fmt.Errorf("session persistence disabled")
	}
	if strings.TrimSpace(id) == "" {
		return "", SessionActionNone, fmt.Errorf("session id is required")
	}
	wasCurrent := id == a.SessionID
	if err := a.SessionStore.Delete(a.WorkingDir, id); err != nil {
		return "", SessionActionNone, err
	}
	if wasCurrent {
		if strings.TrimSpace(newSessionID) == "" {
			return "", SessionActionNone, fmt.Errorf("session id is required")
		}
		a.SessionID = newSessionID
		a.ResetSessionState()
		a.FlushSession()
		out := fmt.Sprintf("Deleted session %s (was current — started new session %s).", id, newSessionID)
		return AppendContextBrief(ctx, a, out), SessionActionClearChat, nil
	}
	return fmt.Sprintf("Deleted session %s.", id), SessionActionNone, nil
}

func (a *Agent) formatSessionList() (string, []SessionInfo, error) {
	if a.SessionStore == nil {
		return "Session persistence is disabled.", nil, nil
	}
	list, err := a.SessionStore.List(a.WorkingDir)
	if err != nil {
		return "", nil, err
	}
	if len(list) == 0 {
		return "No saved sessions.", list, nil
	}
	var b strings.Builder
	b.WriteString("Saved sessions:\n")
	for _, s := range list {
		fmt.Fprintf(&b, "  %s  (%d msgs)", s.ID, s.MessageCount)
		if s.Label != "" {
			fmt.Fprintf(&b, "  \"%s\"", s.Label)
		}
		if s.ID == a.SessionID {
			b.WriteString("  ← current")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nUse: resume <id>  |  resume latest  |  resume del <id>")
	return b.String(), list, nil
}

// FormatSessionListForUI returns saved sessions without the slash-command help text.
func (a *Agent) FormatSessionListForUI() (string, []SessionInfo, error) {
	return a.formatSessionList()
}

// ForkSession starts a new session that is a copy of the current conversation
// up to (and including) a specific message. args can be:
//   - "" or "last": fork from the last assistant message
//   - "<N>": fork from the Nth displayed message (0-indexed, counting only user+assistant)
//   - "assistant <N>": fork from the Nth assistant message (0-indexed)
//   - "created <RFC3339Nano>": fork from message with the given CreatedAt timestamp
func (a *Agent) ForkSession(ctx context.Context, args, newSessionID string) error {
	if strings.TrimSpace(newSessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	if len(a.Messages) == 0 {
		return fmt.Errorf("no messages to fork from")
	}

	// Determine the fork index
	idx := -1
	args = strings.TrimSpace(args)
	switch {
	case args == "" || args == "last":
		// Fork from the last assistant message
		for i := len(a.Messages) - 1; i >= 0; i-- {
			if a.Messages[i].Role == "assistant" {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("no assistant message found to fork from")
		}
	case strings.HasPrefix(args, "created "):
		ts := strings.TrimSpace(strings.TrimPrefix(args, "created "))
		createdAt, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return fmt.Errorf("invalid timestamp %q: %v", ts, err)
		}
		for i, m := range a.Messages {
			diff := m.CreatedAt.Sub(createdAt)
			if diff < 0 {
				diff = -diff
			}
			if diff <= time.Millisecond {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("message with timestamp %q not found", ts)
		}
	case strings.HasPrefix(args, "assistant "):
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(args, "assistant ")))
		if err != nil {
			return fmt.Errorf("usage: fork [assistant <N>] — invalid number: %v", err)
		}
		count := 0
		for i, m := range a.Messages {
			if m.Role == "assistant" {
				if count == n {
					idx = i
					break
				}
				count++
			}
		}
		if idx < 0 {
			return fmt.Errorf("assistant message %d not found (only %d assistant messages)", n, count)
		}
	default:
		// Raw index into the Messages array (matches histIdx from the client).
		n, err := strconv.Atoi(args)
		if err != nil || n < 0 || n >= len(a.Messages) {
			return fmt.Errorf("usage: fork [<N> | last | assistant <N> | created <timestamp>] — invalid index %q", args)
		}
		idx = n
	}

	// Save current session (the original branch)
	if a.SessionStore != nil {
		a.FlushSession()
	}

	oldSessionID := a.SessionID

	// Copy messages up to and including the fork point.
	forkedMsgs := append([]llm.Message(nil), a.Messages[:idx+1]...)

	// If the fork point is an assistant message with tool calls, strip the
	// tool calls from it so the forked session doesn't have orphaned
	// tool calls with no corresponding results.
	if forkedMsgs[idx].Role == "assistant" && len(forkedMsgs[idx].ToolCalls) > 0 {
		forkedMsgs[idx].ToolCalls = nil
	}

	// Start new session with the truncated history
	a.SessionID = newSessionID
	a.Messages = forkedMsgs
	a.clearTurnUsage()
	a.restoredTokenCounts = nil
	a.UsageAccum = UsageAccumulator{}
	a.resetSaveTracking()
	a.clearViewDriftSnapshot()
	a.SessionLabel = ""
	a.SessionOneshot = false
	if a.PinManager != nil {
		a.PinManager.ClearPins()
	}
	if a.TodoManager != nil {
		a.TodoManager.Clear()
	}

	// Persist new session
	if a.SessionStore != nil {
		a.FlushSession()
		_ = a.SessionStore.TouchSession(a.WorkingDir, oldSessionID)
	}

	return nil
}
