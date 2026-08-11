package agent

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gogen/internal/contextmgr"
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
		// The restored model is unverified until ValidateRestoredModel (or
		// the first turn's re-check) confirms it still exists at the
		// provider; until then it must not be blindly sent to the endpoint.
		a.setModelUnverified(true)
	}

	// Pre-warm the context limit from the snapshot so the first ContextStats
	// call after restore sees the correct max context size immediately,
	// without waiting for the async ValidateRestoredModel refresh.
	if a.Context != nil && snap.ContextLimit > 0 {
		a.Context.SetContextLimit(snap.ContextLimit)
	}

	a.compareViewOnRestore(prevSessionID, newSessionID)
}

// ValidateRestoredModel checks that the restored model still exists at the provider and
// refreshes the context limit. Safe to run in the background after startup.
// Bounded so a hung provider cannot run ListModels + ModelContextLimit
// back-to-back for an unbounded wall time.
func (a *Agent) ValidateRestoredModel(ctx context.Context, model string) {
	ctx, cancel := context.WithTimeout(ctx, validateModelTimeout)
	defer cancel()

	// Context limit first: ModelContextLimit tries /v1/models briefly (local
	// n_ctx), then models.dev — without stacking a full catalog wait + Get.
	// Also refresh when no model is selected yet: the probe performs
	// sole-model auto-select, which must not be skipped just because the
	// context limit is already resolved (e.g. pre-warmed from a session
	// snapshot) — otherwise a single-model provider never gets its model
	// chosen and the session stays stuck on "no model selected".
	if a.Context != nil && (a.ContextLimit() <= 0 || a.Provider.ModelName() == "") {
		a.Context.RefreshAfterModelChange(ctx)
	}
	if model == "" {
		return
	}
	models, err := a.Provider.ListModels(ctx)
	if err != nil {
		// Cannot verify the restored model right now (endpoint down,
		// timeout). Fail open — keep the model — but leave it marked
		// unverified so requireModelSelected re-checks on the first turn
		// instead of sending a possibly-stale model to the endpoint.
		return
	}
	found := false
	for _, m := range models {
		if m.ID == model {
			found = true
			break
		}
	}
	if found {
		// The restored model is still served by this provider.
		a.setModelUnverified(false)
	} else {
		// The restored model no longer exists at the provider. Clear it —
		// unless a concurrent /models selection replaced it in the
		// meantime — then re-probe so a single-model provider auto-selects
		// its sole model immediately instead of leaving the session with
		// no model until the next turn.
		if a.Provider.ModelName() == model {
			_ = a.Provider.SetModel("")
		}
		a.setModelUnverified(false)
		if a.Provider.ModelName() == "" {
			_, _ = a.Provider.ModelContextLimit(ctx)
		}
	}
	// Let hosts refresh their UI: the restored model may have been cleared
	// or replaced by sole-model auto-select.
	if a.OnModelChanged != nil {
		a.OnModelChanged()
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

// persistSession marks the session dirty and writes to disk only if the
// minimum interval since the last write has elapsed.  This coalesces
// rapid-fire saves during multi-tool turns into at most one write per
// persistMinInterval.  For final boundaries (turn complete, errors,
// context cancellation) use flushSession instead.
func (a *Agent) persistSession() {
	a.sessionDirty.Store(true)
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	// Skip if debounced — no point computing hash or doing I/O.
	// The debounce read must not race a concurrent doPersist's write of
	// lastPersistTime, so it runs under persistMu (best-effort timing).
	a.persistMu.Lock()
	debounced := !a.lastPersistTime.IsZero() && time.Since(a.lastPersistTime) < persistMinInterval
	a.persistMu.Unlock()
	if debounced {
		return
	}
	a.doPersist(false)
}

// FlushSession forces an immediate disk write regardless of debounce timing.
// Use at final boundaries: turn complete, errors, context cancellation, and quit.
// Skips full re-tokenization so Ctrl+C / --web shutdown stays snappy on large
// sessions; restored counts are reused when still valid.
func (a *Agent) FlushSession() {
	a.sessionDirty.Store(true)
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	a.doPersist(true)
}

// FlushPending writes any unsaved state to disk but, unlike FlushSession,
// does NOT mark the session dirty — a clean session is left untouched
// (doPersist's dirty-flag check makes it a no-op). The shutdown sweep uses
// this instead of FlushSession: forcing a write on every clean session
// re-stamped each one's Updated timestamp with ~now in sweep order (the
// focused session first, so it received the OLDEST stamp), which destroyed
// the recency ordering List/LatestID rely on after a restart — the
// saved-session list reshuffled and the session active at shutdown was
// demoted instead of restored as current. Dirty sessions (an unsaved turn,
// pending metadata change) still write exactly as before, so no state is at
// risk.
func (a *Agent) FlushPending() {
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	a.doPersist(true)
}

// doPersist is the actual write — called by persistSession/flushSession.
// Callers (persistSession, FlushSession) already validate SessionStore/SessionID;
// this method only checks the dirty flag.
//
// It uses incremental delta saves when only a few messages have been added
// since the last full snapshot, avoiding full JSON serialization on every
// 5-second debounce tick.  Importantly, lastSavedMsgCount is NOT advanced on
// incremental saves, so each delta always contains ALL messages since the last
// full snapshot — making the delta file self-contained and crash-safe.
// When skipTokenCounts is true (FlushSession), avoid cl100k re-tokenization.
func (a *Agent) doPersist(skipTokenCounts bool) {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	// Consume the dirty flag up front instead of checking it here and
	// clearing it at the end. Two flushes can be pending concurrently — the
	// turn's persistSession (holding turnMu) and the shutdown/delete/eviction
	// flush paths (no turnMu, e.g. ShutdownSessions after the 2s drain times
	// out) — and a write that snapshots EARLIER state must not clear the flag
	// set by a caller whose state it did not include. With Load-then-Store,
	// the earlier writer's trailing Store(false) wiped the later caller's
	// mark, so the later (possibly final, pre-exit) doPersist saw
	// dirty==false and returned without writing — the "quit during a running
	// turn still loses the last messages" bug. Swapping the flag at the start
	// (under persistMu) makes the writer consume exactly the mutations
	// published before it snapshotted; anything marked during the write
	// stays set and is picked up by the next flush.
	if !a.sessionDirty.Swap(false) {
		return
	}
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	// Extend cached token counts to cover any new messages so the full
	// snapshot path can reuse them instead of re-tokenizing everything.
	a.extendTokenCounts()

	// Snapshot the conversation and label under statsMu: web probes read
	// them without turnMu, so doPersist must not touch the live
	// fields outside the lock. The clone is deep (ToolCalls included) so
	// the snapshot cannot race a concurrent in-place stabilization, and it
	// is safe to tokenize and serialize after releasing the lock.
	a.statsMu.RLock()
	msgs := cloneMessagesShallow(a.Messages)
	label := a.SessionLabel
	labelRenamed := a.labelRenamed
	countsEpoch := a.countsEpoch
	tokenCounts := append([]int(nil), a.tokenCounts...)
	// Mode/thinking/oneshot are written under statsMu (SetMode,
	// SetThinkingLevel, RestoreSessionLocal), so read them under the same
	// lock: doPersist also runs on the shutdown flush path with no turnMu,
	// and an unlocked read there would race a concurrent SetMode or
	// SetThinkingLevel from a still-running turn's command handler.
	mode := a.Mode.String()
	thinking := string(a.ThinkingLevel)
	oneshot := a.SessionOneshot
	workingDir := a.WorkingDir
	a.statsMu.RUnlock()
	count := len(msgs)
	profile := a.ensureProjectProfile()
	model := a.CurrentModel()
	curMeta := persistMeta{
		label:    label,
		mode:     mode,
		model:    model,
		thinking: thinking,
		oneshot:  oneshot,
		profile:  profile,
		todos:    todoSnapshot(a.TodoManager),
	}

	// Safety: if the message list was truncated since last save (e.g.
	// compaction, error rollback), force a full snapshot.
	if a.lastSavedMsgCount > count {
		a.lastSavedMsgCount = 0
	}

	// Decide: full snapshot or incremental delta?
	// Full snapshot on first save, when more than 5 new messages have
	// arrived, or when it's been >30s since the last full snapshot.
	// Also when non-message metadata (label, mode, model, thinking level,
	// oneshot, project profile, todos) changed since the last full snapshot:
	// incremental deltas do not carry those fields, so a quit before the
	// next full save would silently drop the change.
	needsFullSave := a.lastSavedMsgCount == 0 ||
		count-a.lastSavedMsgCount > 5 ||
		time.Since(a.lastFullSaveTime) > 30*time.Second ||
		!reflect.DeepEqual(curMeta, a.lastMeta)

	if needsFullSave {
		snap := SessionSnapshot{
			WorkingDir:     workingDir,
			Model:          model,
			Mode:           mode,
			ThinkingLevel:  thinking,
			Oneshot:        oneshot,
			Label:          label,
			LabelRenamed:   labelRenamed,
			ProjectProfile: profile,
			Todos:          curMeta.todos,
			Messages:       msgs,
			ContextLimit:   a.ContextLimit(),
		}
		if len(tokenCounts) == len(msgs) {
			snap.TokenCounts = tokenCounts
		} else if a.Context != nil && !skipTokenCounts {
			snap.TokenCounts = a.Context.TokenCounts(msgs)
			// Backfill the in-memory cache so the next save or context probe
			// reuses these counts instead of re-tokenizing. The epoch guard
			// drops the result if the message list changed underneath us.
			a.statsMu.Lock()
			if a.countsEpoch == countsEpoch && len(a.tokenCounts) < len(msgs) {
				a.tokenCounts = append(a.tokenCounts, snap.TokenCounts[len(a.tokenCounts):]...)
			}
			a.statsMu.Unlock()
		}
		if err := a.SessionStore.Save(a.SessionID, snap); err != nil {
			log.Printf("session save failed (id=%s): %v", a.SessionID, err)
			a.lastPersistErr = err
			return
		}
		a.lastSavedMsgCount = count
		a.lastFullSaveTime = time.Now()
		a.lastMeta = curMeta
	} else {
		// Incremental: only serialise new messages since the last full save.
		newMsgs := msgs[a.lastSavedMsgCount:]
		var newCounts []int
		if a.Context != nil && !skipTokenCounts {
			if len(tokenCounts) >= count {
				// extendTokenCounts ran above, so the in-memory cache already
				// covers these messages; reuse it instead of re-tokenizing
				// the same content a second time.
				newCounts = append([]int(nil), tokenCounts[a.lastSavedMsgCount:count]...)
			} else {
				newCounts = make([]int, len(newMsgs))
				for i := range newMsgs {
					newCounts[i] = contextmgr.ComputeMessageTokens(newMsgs[i])
				}
			}
		}
		deltaSnap := SessionSnapshot{
			WorkingDir:  workingDir,
			Oneshot:     oneshot,
			Label:       label,
			Messages:    newMsgs,
			TokenCounts: newCounts,
		}
		if err := a.SessionStore.AppendMessages(a.SessionID, deltaSnap, count); err != nil {
			log.Printf("session delta save failed (id=%s): %v", a.SessionID, err)
			a.lastPersistErr = err
			return
		}
		// Do NOT advance lastSavedMsgCount here.  The delta file is
		// overwritten on each incremental save and must always contain ALL
		// messages since the last full snapshot.  Advancing lastSavedMsgCount
		// would make the next delta save include only the newest messages,
		// and a crash between increments would permanently lose the earlier
		// batches.  The full-save thresholds (5 new messages or 30 s) will
		// trigger a full snapshot soon enough, at which point lastSavedMsgCount
		// is updated.
		_ = count // referenced for clarity; not saved until next full snapshot
	}

	a.lastPersistErr = nil
	a.lastPersistTime = time.Now()
}

// resetSaveTracking resets the incremental-save counters so the next
// doPersist writes a full snapshot. Call after any operation that
// truncates or replaces a.Messages (compaction, session restore, etc.).
func (a *Agent) resetSaveTracking() {
	// Serialize against a concurrent doPersist so the counters cannot change
	// mid-write: without this a shutdown flush could clone messages, then the
	// turn's truncate+reset lands, and the flush writes the pre-truncate
	// state as the final one.
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.lastSavedMsgCount = 0
	a.lastFullSaveTime = time.Time{}
}

// appendMessage appends one message to the conversation and, when the token
// count cache is complete (covers every previous message), extends it in the
// same critical section so the message list and the counts cache stay
// consistent for concurrent readers. This is the only way Messages grows
// during a turn. Thread-safe: ContextStats and SnapshotMessages snapshot
// Messages + counts under statsMu while the turn goroutine appends. Leaf
// lock: never acquire turnMu under it.
func (a *Agent) appendMessage(m llm.Message) {
	a.statsMu.Lock()
	a.Messages = append(a.Messages, m)
	if a.tokenCounts != nil && len(a.tokenCounts) == len(a.Messages)-1 {
		a.tokenCounts = append(a.tokenCounts,
			contextmgr.ComputeMessageTokens(m))
	}
	a.statsMu.Unlock()
}

// replaceMessages swaps the conversation wholesale and invalidates the cached
// token counts (compaction, session restore, fork, reset). Publishing the new
// slice and clearing the counts in one critical section means a concurrent
// ContextStats never pairs new messages with stale counts. Leaf lock.
func (a *Agent) replaceMessages(msgs []llm.Message) {
	a.statsMu.Lock()
	a.Messages = msgs
	a.tokenCounts = nil
	a.countsEpoch++
	a.statsMu.Unlock()
}

// replaceMessagesWithCounts swaps the conversation wholesale and publishes
// pre-computed per-message token counts in the same critical section. Unlike
// replaceMessages (which clears the cache), this keeps the fast
// shouldCompactUsingCounts path valid immediately — the caller computes the
// counts before publishing, which is cheap for a compaction because the
// conversation just shrank. Leaf lock.
func (a *Agent) replaceMessagesWithCounts(msgs []llm.Message, counts []int) {
	a.statsMu.Lock()
	a.Messages = msgs
	a.tokenCounts = counts
	a.countsEpoch++
	a.statsMu.Unlock()
}

// restoreMessages publishes a restored session's messages together with their
// pre-computed token counts and marks every message as already-stabilized
// (persisted ArgsStr). One atomic publish so concurrent readers never observe
// partially-initialized messages. Takes ownership of msgs (no defensive copy).
// Leaf lock.
func (a *Agent) restoreMessages(msgs []llm.Message, counts []int) {
	a.statsMu.Lock()
	a.Messages = msgs
	a.tokenCounts = counts
	a.countsEpoch++
	for i := range a.Messages {
		m := &a.Messages[i]
		m.ArgsStabilized = true
		// Recompute the ArgsJSONValid memo for restored tool calls: the
		// persisted ArgsStr is replayed byte-identically (never pinned or
		// trimmed here), so the flag is set only when ArgsStr is already the
		// exact trimmed valid wire bytes. A single validation per load beats
		// re-validating every restored tool call on every request.
		for j := range m.ToolCalls {
			tc := &m.ToolCalls[j]
			s := strings.TrimSpace(tc.ArgsStr)
			tc.ArgsJSONValid = tc.ArgsStr != "" && tc.ArgsStr == s && json.Valid([]byte(s))
		}
	}
	a.statsMu.Unlock()
}

// truncateMessages removes the last n messages (rollback paths) and trims the
// cached token counts to match, keeping the fast SnapshotWithCounts path valid
// after a rollback. Caller must guarantee n <= len(a.Messages). Leaf lock.
func (a *Agent) truncateMessages(n int) {
	a.statsMu.Lock()
	a.Messages = a.Messages[:len(a.Messages)-n]
	if a.tokenCounts != nil && len(a.tokenCounts) > len(a.Messages) {
		a.tokenCounts = a.tokenCounts[:len(a.Messages)]
	}
	a.countsEpoch++
	a.statsMu.Unlock()
}

// SnapshotMessages returns a copy of the current conversation messages that
// is safe to read after the lock is released: unstabilized ToolCalls are
// deep-copied, stabilized ones are shared (see cloneMessagesShallow). Used by the
// web server for history snapshots without holding the turn lock.
func (a *Agent) SnapshotMessages() []llm.Message {
	a.statsMu.RLock()
	msgs := cloneMessagesShallow(a.Messages)
	a.statsMu.RUnlock()
	return msgs
}

// MessageCount returns the current conversation message count. Thread-safe.
func (a *Agent) MessageCount() int {
	a.statsMu.RLock()
	n := len(a.Messages)
	a.statsMu.RUnlock()
	return n
}

// HistoryEpoch returns a counter bumped whenever the conversation is replaced
// wholesale (compaction, session restore, rollback, fork). History snapshots
// stamp it so clients can tell a snapshot that predates a reshape (e.g. a
// compaction that reset message indexes) from one that is merely older than
// the transcript they already rendered. Thread-safe.
func (a *Agent) HistoryEpoch() uint64 {
	a.statsMu.RLock()
	e := a.countsEpoch
	a.statsMu.RUnlock()
	return e
}
