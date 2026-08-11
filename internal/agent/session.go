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
	WorkingDir    string
	Oneshot       bool
	Model         string
	Mode          string
	ThinkingLevel string // persisted thinking level; empty/absent means "off"
	Label         string
	// LabelRenamed is true when Label was set deliberately (RenameSession /
	// the session_rename tool) rather than derived from the first user
	// message. Persisted so the store never regenerates a deliberate rename
	// (see sessionLabel's legacy-50-char migration rule).
	LabelRenamed   bool
	ProjectProfile string
	Todos          *TodoList
	Messages       []llm.Message
	TokenCounts    []int // pre-computed token counts per message (nil if unavailable)
	ContextLimit   int   // resolved context window size (0 = unknown)
}

// SessionPersister stores and loads agent sessions.
type SessionPersister interface {
	Save(id string, snap SessionSnapshot) error
	// AppendMessages writes a cumulative delta (all messages since the last
	// full snapshot); totalMsgCount is the agent's total message count.
	AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error
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

	// Publish messages, their pre-computed token counts, and the stabilized
	// flag atomically (no defensive copy of the message slice).
	a.restoreMessages(snap.Messages, snap.TokenCounts)
	// Token counts are keyed by content fingerprint, so entries from the
	// previous session remain valid as long as the content hasn't changed.
	// Keep the sticky project profile when resuming in the same working
	// directory so the system-prompt prefix stays byte-stable for provider
	// prompt caching. Re-detect only when the directory changed (or the
	// snapshot has no profile).
	if snap.ProjectProfile != "" && sameWorkingDir(snap.WorkingDir, a.WorkingDir) {
		a.statsMu.Lock()
		a.projectProfile = snap.ProjectProfile
		a.statsMu.Unlock()
	} else {
		a.statsMu.Lock()
		a.projectProfile = ""
		a.statsMu.Unlock()
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
	a.statsMu.Lock()
	a.UsageAccum = UsageAccumulator{}
	a.statsMu.Unlock()
	// Reset save tracking — next doPersist will write a full snapshot that
	// merges any existing delta file from a previous process lifetime.
	a.resetSaveTracking()
	a.setSessionLabel(snap.Label)
	// Mode/thinking are read by config snapshots under statsMu (see
	// ModeAndThinkingLevel), so the restore publishes them under the same
	// lock — a concurrent attach's config snapshot would otherwise tear the
	// plain field reads. SessionOneshot is read by doPersist under statsMu,
	// so it is published under the same lock.
	a.statsMu.Lock()
	a.SessionOneshot = snap.Oneshot
	// Restore the rename marker alongside the label: a label that was a
	// deliberate rename before persistence must stay authoritative after a
	// restart (sessionLabel's legacy-50-char migration must not clobber it).
	a.labelRenamed = snap.LabelRenamed
	if m, ok := ParseMode(snap.Mode); ok {
		a.Mode = m
	}
	if l := NormalizeThinkingLevel(snap.ThinkingLevel); l != "" {
		a.ThinkingLevel = l
	}
	a.statsMu.Unlock()
	// The provider owns a separate reasoning-effort state (in web mode every
	// session gets a fresh provider seeded with the workspace default). Publish
	// the restored level to it so non-turn calls (e.g. /compact summarization
	// via GenerateResponse) and the first turn agree with the restored session
	// instead of the provider's stale seed. StreamProcessInput re-syncs every
	// turn as a safety net.
	if a.Provider != nil {
		a.Provider.SetThinkingLevel(string(a.ThinkingLevel))
	}
	// Seed the metadata fingerprint from the restored snapshot so the first
	// post-restore save compares against what was actually restored (the
	// save is a full one regardless — resetSaveTracking above — but this
	// keeps lastMeta truthful for the saves after that). lastMeta is
	// read/written by doPersist under persistMu, so the seed publishes under
	// the same lock. Seeded after the field applies above so a concurrent
	// doPersist never compares new metadata against a stale seed.
	a.persistMu.Lock()
	// Normalize the seed exactly as the fields above were normalized, so the
	// first post-restore doPersist compares like for like: an old snapshot
	// persisted "max" restores the field as "high", and TodoManager.Replace
	// (run earlier in this restore) normalizes NextID — seeding the raw
	// values would make lastMeta differ from the restored state until the
	// first full save rewrites it. (Cosmetic: that save is full anyway via
	// resetSaveTracking, but the seed should be truthful for the saves
	// after it.)
	seedMode, seedThinking := snap.Mode, snap.ThinkingLevel
	if m, ok := ParseMode(snap.Mode); ok {
		seedMode = m.String()
	}
	if l := NormalizeThinkingLevel(snap.ThinkingLevel); l != "" {
		seedThinking = string(l)
	}
	a.lastMeta = persistMeta{
		label:    snap.Label,
		mode:     seedMode,
		model:    snap.Model,
		thinking: seedThinking,
		oneshot:  snap.Oneshot,
		profile:  snap.ProjectProfile,
		todos:    todoSnapshot(a.TodoManager),
	}
	a.persistMu.Unlock()
	if snap.Model != "" {
		_ = a.Provider.SetModel(snap.Model)
	}

	// Pre-warm the context limit from the snapshot so the first ContextStats
	// call after restore sees the correct max context size immediately,
	// without waiting for the async ValidateRestoredModel refresh.
	if a.Context != nil && snap.ContextLimit > 0 {
		a.Context.SetContextLimit(snap.ContextLimit)
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

// RestoreSession loads messages, mode, and model from a snapshot and makes id
// the agent's current session. It is the shared restore core used by
// main.go's startup restore, resumeSessionByID, and the session agent
// factory; model validation stays with the caller (async, so the UI is not
// blocked on provider ListModels).
func (a *Agent) RestoreSession(snap SessionSnapshot, id string) {
	a.RestoreSessionLocal(snap, id)
	a.SessionID = id
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

// setSessionLabel stores the session label under statsMu: web probes read it
// without turnMu (agentConfigMsgBasic, contextMsg) while the turn
// goroutine may derive it from the first user message. Clears the rename
// marker: derived, restored, and cleared labels are not renames by
// themselves (restore re-establishes the marker from the persisted snapshot
// when it was one).
func (a *Agent) setSessionLabel(label string) {
	a.statsMu.Lock()
	a.SessionLabel = label
	a.labelRenamed = false
	a.statsMu.Unlock()
}

// setSessionLabelRenamed is setSessionLabel for deliberate renames
// (RenameSession / the session_rename tool): the marker is persisted so the
// store never regenerates the label from the conversation, even when it
// happens to look like a legacy 50-char truncation of the first user message.
func (a *Agent) setSessionLabelRenamed(label string) {
	a.statsMu.Lock()
	a.SessionLabel = label
	a.labelRenamed = true
	a.statsMu.Unlock()
}

// SessionLabelSnapshot returns the current session label. Thread-safe.
func (a *Agent) SessionLabelSnapshot() string {
	a.statsMu.RLock()
	l := a.SessionLabel
	a.statsMu.RUnlock()
	return l
}

// RenameSession sets a user-visible label for the current session and persists it.
func (a *Agent) RenameSession(label string) (string, error) {
	a.setSessionLabelRenamed(label)
	a.FlushSession()
	return "Session label set to " + label, nil
}
