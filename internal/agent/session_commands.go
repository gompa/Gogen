package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/contextmgr"
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

// resetSessionState clears all session-related state for starting fresh.
// This is used when creating a new session or replacing a deleted current session.
func (a *Agent) resetSessionState() {
	a.Messages = nil
	// Wipe token-count cache entries — old content is gone and new
	// conversations start from empty.
	contextmgr.InvalidateTokenCache()
	a.lastTurnUsage = nil
	a.UsageAccum = UsageAccumulator{}
	a.SessionLabel = ""
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
		return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: a.Messages}, true, nil
	}
	out, err := a.resumeSessionByID(ctx, args)
	if err != nil {
		return SessionCommandResult{}, true, err
	}
	return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: a.Messages}, true, nil
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
	a.resetSessionState()
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
	a.RestoreSessionLocal(snap)
	a.SessionID = id
	// Persist immediately so the resumed session gets a fresh UpdatedAt
	// timestamp and appears at the top of the session sidebar list.
	a.FlushSession()
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
		a.resetSessionState()
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
