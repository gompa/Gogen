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
	trimmed := strings.TrimSpace(input)
	switch trimmed {
	case "/new", "new":
		out, err := a.startNewSession(newSessionID)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: AppendContextBrief(ctx, a, out), Action: SessionActionClearChat}, true, nil
	case "sessions", "/sessions", "resume", "/resume":
		out, sessions, err := a.formatSessionList()
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Sessions: sessions}, true, nil
	}
	if strings.HasPrefix(trimmed, "resume ") || strings.HasPrefix(trimmed, "/resume ") {
		id := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/resume"), "resume"))
		if id == "" {
			out, sessions, err := a.formatSessionList()
			if err != nil {
				return SessionCommandResult{}, true, err
			}
			return SessionCommandResult{Output: out, Sessions: sessions}, true, nil
		}
		if strings.HasPrefix(id, "del ") || id == "del" {
			delID := strings.TrimSpace(strings.TrimPrefix(id, "del"))
			if delID == "" {
				return SessionCommandResult{}, true, fmt.Errorf("usage: resume del <id>")
			}
			out, action, err := a.deleteSessionByID(ctx, delID, newSessionID)
			if err != nil {
				return SessionCommandResult{}, true, err
			}
			return SessionCommandResult{Output: out, Action: action}, true, nil
		}
		if id == "latest" {
			out, err := a.resumeLatestSession(ctx)
			if err != nil {
				return SessionCommandResult{}, true, err
			}
			return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: a.Messages}, true, nil
		}
		out, err := a.resumeSessionByID(ctx, id)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: a.Messages}, true, nil
	}
	return SessionCommandResult{}, false, nil
}

func (a *Agent) startNewSession(newID string) (string, error) {
	if strings.TrimSpace(newID) == "" {
		return "", fmt.Errorf("session id is required")
	}
	oldID := a.SessionID
	if a.SessionStore != nil {
		a.persistSession()
	}
	a.SessionID = newID
	a.Messages = nil
	// Wipe any token-count cache entries — the old message pointers are
	// being released and new conversations start from empty.
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
	if a.SessionStore != nil {
		a.persistSession()
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
	a.RestoreSession(ctx, snap)
	a.SessionID = id
	label := llm.SessionLabel(snap.Messages, llm.DefaultSessionLabelMaxLen)
	var out string
	if label != "" {
		out = fmt.Sprintf("Resumed session %s (%d messages): \"%s\"", id, len(snap.Messages), label)
	} else {
		out = fmt.Sprintf("Resumed session %s (%d messages).", id, len(snap.Messages))
	}
	return AppendContextBrief(ctx, a, out), nil
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
		a.Messages = nil
		// Old message pointers are being released; drop cached token counts.
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
		a.persistSession()
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
