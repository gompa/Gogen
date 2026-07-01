package agent

import (
	"context"

	"gogen/internal/llm"
)

// SessionSnapshot is persisted conversation state.
type SessionSnapshot struct {
	WorkingDir string
	Model      string
	Mode       string
	Messages   []llm.Message
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

// RestoreSession loads messages, mode, and model from a snapshot.
func (a *Agent) RestoreSession(ctx context.Context, snap SessionSnapshot) {
	a.Messages = append([]llm.Message(nil), snap.Messages...)
	if m, ok := ParseMode(snap.Mode); ok {
		a.Mode = m
	}
	if snap.Model != "" {
		_ = a.Provider.SetModel(snap.Model)
		models, err := a.Provider.ListModels(ctx)
		if err == nil {
			found := false
			for _, m := range models {
				if m.ID == snap.Model {
					found = true
					break
				}
			}
			if !found {
				_ = a.Provider.SetModel("")
			}
		}
		if a.Context != nil {
			a.Context.RefreshAfterModelChange(ctx)
		}
	}
}
