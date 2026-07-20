package agent

import (
	"context"
	"path/filepath"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// SessionSnapshot is persisted conversation state.
type SessionSnapshot struct {
	WorkingDir     string
	Model          string
	Mode           string
	Label          string
	ProjectProfile string
	Todos          *TodoList
	Messages       []llm.Message
}

// SessionPersister stores and loads agent sessions.
type SessionPersister interface {
	Save(id string, snap SessionSnapshot) error
	LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error)
	List(workingDir string) ([]SessionInfo, error)
	LatestID(workingDir string) (string, error)
	Delete(workingDir, id string) error
}

// SessionInfo describes a saved session entry.
type SessionInfo struct {
	ID           string
	UpdatedAt    string
	MessageCount int
	Label        string
}

// RestoreSessionLocal loads messages, mode, and model from a snapshot without
// contacting the provider. Prefer this at process startup so the UI can come
// up before model validation / context-limit lookup.
func (a *Agent) RestoreSessionLocal(snap SessionSnapshot) {
	a.Messages = append([]llm.Message(nil), snap.Messages...)
	// Cached token counts are keyed by message pointer; restoring gives
	// every message a new address, so any old entries are dead weight.
	contextmgr.InvalidateTokenCache()
	// Keep the sticky project profile when resuming in the same working
	// directory so the system-prompt prefix stays byte-stable for provider
	// prompt caching. Re-detect only when the directory changed (or the
	// snapshot has no profile).
	if snap.ProjectProfile != "" && sameWorkingDir(snap.WorkingDir, a.WorkingDir) {
		a.projectProfile = snap.ProjectProfile
	} else {
		a.projectProfile = ""
	}
	// Pins are not persisted; drop any in-process indices from the previous
	// session so they cannot apply to the restored history.
	if a.PinManager != nil {
		a.PinManager.ClearPins()
	}
	// Todos are session-scoped. Older snapshots without a Todos field restore
	// to an empty list so project-global todos cannot leak across sessions.
	if a.TodoManager != nil {
		a.TodoManager.Replace(snap.Todos)
	}
	a.lastTurnUsage = nil
	a.UsageAccum = UsageAccumulator{}
	a.SessionLabel = snap.Label
	if m, ok := ParseMode(snap.Mode); ok {
		a.Mode = m
	}
	if snap.Model != "" {
		_ = a.Provider.SetModel(snap.Model)
	}
}

// ValidateRestoredModel checks that model still exists at the provider and
// refreshes the context limit. Safe to run in the background after startup.
func (a *Agent) ValidateRestoredModel(ctx context.Context, model string) {
	if model != "" {
		models, err := a.Provider.ListModels(ctx)
		if err == nil {
			found := false
			for _, m := range models {
				if m.ID == model {
					found = true
					break
				}
			}
			if !found && a.Provider.ModelName() == model {
				_ = a.Provider.SetModel("")
			}
		}
	}
	if a.Context != nil {
		a.Context.RefreshAfterModelChange(ctx)
	}
}

// RestoreSession loads messages, mode, and model from a snapshot, then
// validates the model against the provider (network).
func (a *Agent) RestoreSession(ctx context.Context, snap SessionSnapshot) {
	a.RestoreSessionLocal(snap)
	a.ValidateRestoredModel(ctx, snap.Model)
}

// sameWorkingDir reports whether two working-directory paths refer to the same location.
// An empty snapshot dir is treated as matching (older sessions / same-store loads).
func sameWorkingDir(snapDir, currentDir string) bool {
	if snapDir == "" {
		return true
	}
	a, errA := filepath.Abs(snapDir)
	b, errB := filepath.Abs(currentDir)
	if errA != nil || errB != nil {
		return filepath.Clean(snapDir) == filepath.Clean(currentDir)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// RenameSession sets a user-visible label for the current session and persists it.
func (a *Agent) RenameSession(label string) (string, error) {
	a.SessionLabel = label
	a.persistSession()
	return "Session label set to " + label, nil
}
