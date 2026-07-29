package agent

import (
	"context"
	"path/filepath"
	"time"

	"gogen/internal/llm"
)

// validateModelTimeout bounds the model validation call after session restore.
const validateModelTimeout = 12 * time.Second

// SessionSnapshot is persisted conversation state.
type SessionSnapshot struct {
	WorkingDir     string
	Oneshot        bool
	Model          string
	Mode           string
	ThinkingLevel  string // persisted thinking level; empty/absent means "off"
	Label          string
	ProjectProfile string
	Todos          *TodoList
	Messages       []llm.Message
	TokenCounts    []int // pre-computed token counts per message (nil if unavailable)
	ContextLimit   int   // resolved context window size (0 = unknown)
}

// SessionPersister stores and loads agent sessions.
type SessionPersister interface {
	Save(id string, snap SessionSnapshot) error
	AppendMessages(id string, snap SessionSnapshot) error
	LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error)
	List(workingDir string) ([]SessionInfo, error)
	LatestID(workingDir string) (string, error)
	Delete(workingDir, id string) error
	// TouchSession updates only the timestamp metadata without rewriting messages.
	TouchSession(workingDir, id string) error
}

// SessionInfo describes a saved session entry.
type SessionInfo struct {
	ID           string
	Oneshot      bool
	UpdatedAt    string
	MessageCount int
	Label        string
	// RawLabel holds the full first user message (untruncated) for tooltip
	// display, while Label is the short preview shown in the sidebar.
	RawLabel string
}

// RestoreSessionLocal loads messages, mode, and model from a snapshot without
// contacting the provider. Prefer this at process startup so the UI can come
// up before model validation / context-limit lookup.
//
// newSessionID is the id being resumed into (may be empty at process startup).
// Debug builds with GOGEN_DEBUG_COMPARE_MESSAGES compare the previous session's
// wire view against the restored view (compiled out of production binaries).
func (a *Agent) RestoreSessionLocal(snap SessionSnapshot, newSessionID string) {
	prevSessionID := a.SessionID

	// Take ownership of the snapshot's message slice — no defensive copy.
	a.Messages = snap.Messages
	// Token counts are keyed by content fingerprint, so entries from the
	// previous session remain valid as long as the content hasn't changed.
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
	a.clearTurnUsage()
	a.UsageAccum = UsageAccumulator{}
	// Reset save tracking — next doPersist will write a full snapshot that
	// merges any existing delta file from a previous process lifetime.
	a.resetSaveTracking()
	a.SessionLabel = snap.Label
	a.SessionOneshot = snap.Oneshot
	if m, ok := ParseMode(snap.Mode); ok {
		a.Mode = m
	}
	if snap.ThinkingLevel != "" {
		if l, ok := ParseThinkingLevel(snap.ThinkingLevel); ok {
			a.ThinkingLevel = l
		}
	}
	if snap.Model != "" {
		_ = a.Provider.SetModel(snap.Model)
	}

	// Pre-warm the token-count cache from the snapshot so subsequent
	// ContextStats calls can avoid re-tokenizing every message (expensive
	// for large sessions). Cleared when messages are modified (append,
	// compaction, session reset).
	a.restoredTokenCounts = snap.TokenCounts

	// Pre-warm the context limit from the snapshot so the first ContextStats
	// call after restore sees the correct max context size immediately,
	// without waiting for the async ValidateRestoredModel refresh.
	if a.Context != nil && snap.ContextLimit > 0 {
		a.Context.SetContextLimit(snap.ContextLimit)
	}

	// Snapshot messages were persisted with stable ArgsStr, so mark them
	// as stabilized to avoid re-serializing on the next turn.
	for i := range a.Messages {
		a.Messages[i].ArgsStabilized = true
	}

	a.compareViewOnRestore(prevSessionID, newSessionID)
}

// ValidateRestoredModel checks that model still exists at the provider and
// refreshes the context limit. Safe to run in the background after startup.
// Bounded so a hung provider cannot run ListModels + ModelContextLimit
// back-to-back for an unbounded wall time.
func (a *Agent) ValidateRestoredModel(ctx context.Context, model string) {
	ctx, cancel := context.WithTimeout(ctx, validateModelTimeout)
	defer cancel()

	// Context limit first: ModelContextLimit tries /v1/models briefly (local
	// n_ctx), then models.dev — without stacking a full catalog wait + Get.
	if a.Context != nil && a.ContextLimit() <= 0 {
		a.Context.RefreshAfterModelChange(ctx)
	}
	if model == "" {
		return
	}
	models, err := a.Provider.ListModels(ctx)
	if err != nil {
		return
	}
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

// RestoreSession loads messages, mode, and model from a snapshot, then
// validates the model against the provider (network).
func (a *Agent) RestoreSession(ctx context.Context, snap SessionSnapshot) {
	a.RestoreSessionLocal(snap, a.SessionID)
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
	a.FlushSession()
	return "Session label set to " + label, nil
}
