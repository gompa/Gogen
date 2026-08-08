package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/projectfile"
)

// persistMinInterval is the minimum time between debounced session writes.
// Final boundaries (turn complete, errors) bypass this via flushSession().
const persistMinInterval = 5 * time.Second

type Agent struct {
	Provider llm.LLMProvider
	Executor *Executor

	// Conversation state
	Context     *contextmgr.Manager
	Messages    []llm.Message
	PinManager  *PinManager
	TodoManager *TodoManager

	// Session persistence
	SessionStore   SessionPersister
	SessionID      string
	SessionLabel   string
	SessionOneshot bool // true if this session was created by a single-prompt (-p) invocation
	UsageAccum     UsageAccumulator

	// bgMu guards bgJobs: shell commands started with execute_command
	// background=true. Jobs outlive the turn that started them (they are
	// owned by the session, not the turn) and are killed when the session
	// closes (Close) or individually via background_job_cancel.
	bgMu   sync.Mutex
	bgJobs map[string]*BackgroundJob

	// statsMu serializes the agent state that ContextStats/SnapshotMessages
	// read without the session turnMu: Messages, the cached token counts
	// (tokenCounts), the API-usage baseline (lastTurnUsage,
	// apiBaseline*), projectProfile, UsageAccum, and SessionLabel. Every
	// mutation of these fields from any goroutine must take statsMu. Leaf
	// lock: while holding it, never call out to code that takes turnMu.
	// The reverse order does occur — server paths call
	// SessionLabelSnapshot/ContextStats while holding turnMu — so
	// statsMu critical sections must stay short and never block on I/O or
	// other locks.
	statsMu sync.RWMutex

	lastTurnUsage *llm.Usage
	// apiBaselinePromptTokens and apiBaselineMsgCount let ContextStats use the
	// API's exact prompt_tokens as the authoritative baseline for Snapshot.Used,
	// only estimating messages added after the last API round.
	apiBaselinePromptTokens, apiBaselineMsgCount int
	lastPersistErr                               error
	// sessionDirty tracks whether in-memory state differs from disk.
	// TUI: single owner goroutine. Web server: the per-session turnMu
	// serializes access across WebSocket clients (see internal/server), but the quit
	// flush (ShutdownSessions) and a running turn's flushes can both mark and
	// clear it concurrently — hence atomic (benign races on a plain bool were
	// still data races under -race).
	sessionDirty    atomic.Bool
	lastPersistTime time.Time // timestamp of last actual disk write
	// lastSavedMsgCount tracks how many messages were included in the last
	// full snapshot save. Used by doPersist to decide between full and
	// incremental delta saves, avoiding full JSON serialization every 5s.
	lastSavedMsgCount int
	lastFullSaveTime  time.Time // when the last full snapshot was written

	// persistMu serializes doPersist executions. The turn goroutine (holding
	// turnMu) and the shutdown/delete/eviction flush paths (no turnMu) can
	// call FlushSession concurrently — e.g. ShutdownSessions flushes after
	// the 2s drain times out while the turn is still running. Without this
	// lock two doPersists would interleave their message snapshots, counter
	// reads, and store writes, leaving a torn (snapshot, delta, index)
	// combination on disk that LoadInWorkingDir then drops or double-merges
	// — the "quit during a running turn loses history" bug. Serializing the
	// whole write keeps every pair consistent: the later writer re-reads the
	// counters the earlier one updated. Leaf lock: never held while
	// acquiring turnMu, and no code calls FlushSession/persistSession
	// while holding it (doPersist takes statsMu inside it, so statsMu holders
	// must not flush — they don't).
	persistMu sync.Mutex

	// lastMeta is the session metadata written by the last full snapshot
	// save. doPersist forces a full save (instead of an incremental delta)
	// when any of these changed since the last full snapshot: incremental
	// deltas only carry messages, so label/mode/model/thinking/oneshot/
	// profile/todo changes would otherwise be silently lost if the process
	// quit (or crashed) before the next full save.
	lastMeta persistMeta

	// DebugCompareMessages enables view-fingerprint comparison across turns
	// and session restores (GOGEN_DEBUG_COMPARE_MESSAGES). Only effective in
	// binaries built with `-tags debug`; production builds compile the
	// detector out (see view_drift_release.go).
	DebugCompareMessages bool
	lastViewMessages     []llm.Message // debug builds only; unused in release

	// tokenCounts caches per-message token estimates aligned 1:1 with
	// Messages[0:len(tokenCounts)] (a prefix). When len(tokenCounts) ==
	// len(Messages) every message has a cached count and ContextStats /
	// ShouldCompact can avoid re-tokenizing the whole conversation. The cache
	// is filled incrementally: appendMessage extends a complete cache, and
	// ContextStats / doPersist backfill the missing suffix on demand. It is
	// cleared (nil) whenever the message list is replaced wholesale
	// (compaction, restore, fork, reset, rollback).
	//
	// countsEpoch is bumped on every wholesale message-list change so a
	// concurrent ContextStats that computed counts for an older snapshot can
	// detect the list moved under it and skip publishing stale counts.
	tokenCounts []int
	countsEpoch uint64

	// ThinkingLevel controls how much reasoning/thinking the model should use.
	// When "off", no thinking parameter is sent to the API.
	ThinkingLevel ThinkingLevel

	// Runtime / project
	WorkingDir        string
	Mode              Mode
	GlobalMode        bool
	ProjectGuidelines string
	ProjectFilePath   string
	TestCommand       string
	LintCommand       string
	projectProfile    string
	MCPRegistry       MCPToolRegistry
	// toolMu guards toolHandlers. SetToolHandlers is called at construction
	// (server startup / per-session agent factory) before any turn runs, but
	// executeTool reads the map on every tool call, so the swap is published
	// under the lock to keep the read/write race-free by construction.
	toolMu       sync.RWMutex
	toolHandlers map[string]ToolHandler

	// patchFailStreak counts consecutive patch_file failures so the agent loop
	// can steer the model away from retrying the same stale diff indefinitely.
	patchFailStreak atomic.Int32
}

// persistMeta is the set of session fields that only the full snapshot path
// persists (incremental deltas carry messages plus a label that loads
// ignore). Comparing the current values against the last full save detects
// changes that must force a full save — see lastMeta.
type persistMeta struct {
	label    string
	mode     string
	model    string
	thinking string
	oneshot  bool
	profile  string
	todos    *TodoList
}

func NewAgent(provider llm.LLMProvider, executor *Executor, ctxMgr *contextmgr.Manager) *Agent {
	return &Agent{
		Provider:      provider,
		Executor:      executor,
		Context:       ctxMgr,
		Messages:      []llm.Message{},
		WorkingDir:    executor.GetWorkingDir(),
		Mode:          ModeAct,
		GlobalMode:    false,
		ThinkingLevel: ThinkingOff,
		toolHandlers:  BuiltinToolHandlers(),
	}
}

func (a *Agent) SetProjectContext(path, guidelines, testCommand, lintCommand string) {
	a.ProjectFilePath = path
	a.ProjectGuidelines = guidelines
	a.TestCommand = strings.TrimSpace(testCommand)
	a.LintCommand = strings.TrimSpace(lintCommand)
	a.statsMu.Lock()
	a.projectProfile = ""
	a.statsMu.Unlock()
}

func (a *Agent) SetMCPRegistry(reg MCPToolRegistry) {
	a.MCPRegistry = reg
}

// SaveConfig writes the effective configuration to the project file.
// Returns the config path, guidelines path, and any error.
func (a *Agent) SaveConfig(cfg *config.Config, includeSecrets bool) (cfgPath, guidelinesPath string, err error) {
	if cfg == nil {
		return "", "", fmt.Errorf("config not available")
	}
	effective := *cfg
	effective.OpenAIModel = a.CurrentModel()
	if a.GlobalMode {
		cfgPath = projectfile.GlobalConfigPath()
		guidelinesPath = "" // no guidelines file in global mode
		err = projectfile.SaveGlobalConfig(&effective, projectfile.WriteOptions{IncludeSecrets: includeSecrets})
		if err != nil {
			cfgPath = ""
		}
	} else {
		cfgPath = projectfile.DefaultSavePath(a.WorkingDir)
		guidelinesPath = projectfile.DefaultGuidelinesSavePath(a.WorkingDir)
		err = projectfile.SaveConfig(cfgPath, guidelinesPath, &effective, a.ProjectGuidelines, projectfile.WriteOptions{IncludeSecrets: includeSecrets})
	}
	return
}

// todo ensures the TodoManager is initialized and returns it.
func (a *Agent) todo() (*TodoManager, error) {
	if a.TodoManager == nil {
		return nil, fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager, nil
}

// pinLastUser pins the most recent user message so it survives compaction.
// When no PinManager is configured the tool degrades to a no-op (this only
// happens in tests/custom embeds) so the LLM sees a successful acknowledgement
// rather than a confusing error.
func (a *Agent) pinLastUser() error {
	if a.PinManager == nil {
		return nil
	}
	a.PinManager.PinLastUser(a.Messages)
	return nil
}

func (a *Agent) listPins() string {
	if a.PinManager == nil {
		return "Pin manager is not initialized"
	}
	return a.PinManager.ListPins(a.Messages)
}

func (a *Agent) llmTools() []llm.Tool {
	tools := BuiltinTools()
	if a.MCPRegistry != nil {
		tools = append(tools, a.MCPRegistry.Definitions()...)
	}
	return tools
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
		a.Messages[i].ArgsStabilized = true
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

// cloneMessagesShallow copies a message slice so the result can be read after
// statsMu is released without racing the turn goroutine's in-place
// stabilization (stabilizeToolArgs rewrites ToolCall ArgsStr under statsMu).
// Only unstabilized messages need their ToolCalls deep-copied: stabilization
// is the sole writer of ToolCall fields, it skips messages already marked
// ArgsStabilized, and the wire serializer (messagesToChat) only pins ArgsStr
// for calls whose ArgsStr is still empty/invalid — which stabilization has
// already made valid and trimmed. A message with ArgsStabilized=true therefore
// has ToolCalls that are never mutated again, so sharing their slice is as
// safe as copying it and avoids O(total tool calls) allocation on every
// ContextStats probe / persist snapshot. Callers must hold statsMu (R or W).
func cloneMessagesShallow(msgs []llm.Message) []llm.Message {
	out := append([]llm.Message(nil), msgs...)
	for i := range out {
		if out[i].ArgsStabilized {
			continue
		}
		if len(out[i].ToolCalls) > 0 {
			out[i].ToolCalls = append([]llm.ToolCall(nil), out[i].ToolCalls...)
		}
	}
	return out
}

// cachedProjectProfile returns the sticky project-profile string, or "" when
// none has been detected. Thread-safe: ensureProjectProfile/SetProjectContext/
// SetWorkingDir/RestoreSessionLocal write it under statsMu, and ContextStats
// reads it here so a concurrent turn's profile detection cannot race readers.
func (a *Agent) cachedProjectProfile() string {
	a.statsMu.RLock()
	p := a.projectProfile
	a.statsMu.RUnlock()
	return p
}

// ensureProjectProfile detects and caches the project profile on first use.
// Detection (disk reads) happens outside the lock; the store is double-checked
// so a concurrent ContextStats read never sees a torn value. Called from the
// turn goroutine (prepareMessages) and doPersist.
func (a *Agent) ensureProjectProfile() string {
	if p := a.cachedProjectProfile(); p != "" {
		return p
	}
	// WorkingDir is published under statsMu (SetWorkingDir), and this runs on
	// the shutdown/eviction flush paths with no turnMu, so read it under the
	// lock instead of racing a concurrent working-dir change.
	a.statsMu.RLock()
	wd := a.WorkingDir
	a.statsMu.RUnlock()
	profile := DetectProjectProfile(wd, a.TestCommand, a.LintCommand)
	a.statsMu.Lock()
	if a.projectProfile == "" {
		a.projectProfile = profile
	}
	p := a.projectProfile
	a.statsMu.Unlock()
	return p
}

// extendTokenCounts extends tokenCounts to cover any messages appended since
// the last extension. With appendMessage maintaining the counts inline this is
// normally a no-op; it is kept as a safety net for doPersist so a full
// snapshot can reuse the fast counts path.
func (a *Agent) extendTokenCounts() {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.tokenCounts == nil {
		return
	}
	for len(a.tokenCounts) < len(a.Messages) {
		idx := len(a.tokenCounts)
		a.tokenCounts = append(a.tokenCounts,
			contextmgr.ComputeMessageTokens(a.Messages[idx]))
	}
}

// shouldCompactUsingCounts reports whether auto-compaction should trigger,
// summing the cached per-message token counts when they are complete to avoid
// re-tokenizing the whole conversation on every turn. Falls back to
// Manager.ShouldCompact (a full EstimateTokens pass) when the cache is empty
// or incomplete (e.g. right after a compaction or session restore).
func (a *Agent) shouldCompactUsingCounts() bool {
	if a.Context == nil {
		return false
	}
	a.statsMu.RLock()
	counts := a.tokenCounts
	msgs := a.Messages
	complete := counts != nil && len(counts) == len(msgs)
	a.statsMu.RUnlock()
	if !complete {
		return a.Context.ShouldCompact(msgs)
	}
	if !a.Context.AutoCompactEnabled() {
		return false
	}
	if len(msgs) <= a.Context.CompactKeepRecentMessages()+1 {
		return false
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	return total >= a.Context.CompactBudget()
}

// ConsumePersistError returns and clears the last session save failure, if any.
// Serialized with doPersist via persistMu: doPersist writes lastPersistErr
// under persistMu, and in web mode a no-turnMu flush path (ShutdownSessions,
// sessionDelete, registry eviction) can run concurrently with the turn end
// that consumes the error here.
func (a *Agent) ConsumePersistError() error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	err := a.lastPersistErr
	a.lastPersistErr = nil
	return err
}

// SetWorkingDir updates in-memory working directory fields (agent, executor,
// todos). It does not touch disk or the models.dev cache — call
// AfterWorkingDirChange for that.
func (a *Agent) SetWorkingDir(dir string) {
	// WorkingDir is read by ContextStats and doPersist without the turn lock
	// (web probes, shutdown/eviction flushes), so the publish goes under
	// statsMu together with the projectProfile reset — a plain field write
	// here raced those unlocked readers (data race on a.WorkingDir).
	a.statsMu.Lock()
	a.WorkingDir = dir
	a.projectProfile = ""
	a.statsMu.Unlock()
	if a.Executor != nil {
		a.Executor.SetWorkingDir(dir)
	}
	if a.TodoManager != nil {
		a.TodoManager.SetWorkingDir(dir)
	}
}

// AfterWorkingDirChange persists the session and retargets the models.dev
// cache for the new project dir. Both steps do disk (and possibly background
// network) I/O. The web server calls it under the session's turnMu so it is
// serialized with a concurrent doPersist from a running turn.
func (a *Agent) AfterWorkingDirChange() {
	cacheDir := a.WorkingDir
	if a.GlobalMode {
		cacheDir = projectfile.GlobalDataDir()
	}
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok {
		p.SetModelInfoCacheDir(cacheDir)
	}
	// The session's persisted location follows the working dir
	// (Store.Save/AppendMessages key by snap.WorkingDir). After a dir
	// change the last full snapshot lives in the OLD directory, so an
	// incremental delta here would be written to the NEW directory without
	// its base snapshot — the session becomes unloadable there until the
	// next full save (which may never come if the process quits first).
	// Force a full snapshot into the new directory instead.
	a.resetSaveTracking()
	a.FlushSession()
}

func todoSnapshot(m *TodoManager) *TodoList {
	if m == nil {
		return nil
	}
	return m.Snapshot()
}

// ImportLegacyTodos adopts a project-level `.gogen/todos.json` into the current
// session once, then persists the session so the todos become session-scoped.
func (a *Agent) ImportLegacyTodos() bool {
	if a.TodoManager == nil || !a.TodoManager.ImportLegacyFile() {
		return false
	}
	a.persistTodos()
	return true
}

// persistTodos writes todo changes: with the session when persistence is on,
// otherwise to the legacy project-level todos file.
func (a *Agent) persistTodos() {
	if a.SessionStore != nil && a.SessionID != "" {
		a.FlushSession()
		return
	}
	if a.TodoManager != nil {
		if err := a.TodoManager.saveLegacy(); err != nil {
			log.Printf("todo save failed: %v", err)
		}
	}
}

func finishStreamUI(h *llm.StreamHandlers) {
	if h != nil && h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
}

func ensureToolCallIDs(toolCalls []llm.ToolCall) {
	for j := range toolCalls {
		if toolCalls[j].ID == "" {
			toolCalls[j].ID = newToolCallID()
		}
	}
}

var (
	toolCallIDMu   sync.Mutex
	toolCallIDSeq  uint64
	toolCallIDSeed = uint64(time.Now().UnixNano())
)

func newToolCallID() string {
	toolCallIDMu.Lock()
	toolCallIDSeq++
	seq := toolCallIDSeq
	toolCallIDMu.Unlock()
	return fmt.Sprintf("call_%x_%x", toolCallIDSeed, seq)
}

// prepareMessages builds the LLM view for the next round, compacting history
// at conversation boundaries when auto-compaction triggers. h carries the
// stream handlers for the round; OnCompacting fires before the summarization
// call so the UI can show compaction progress.
func (a *Agent) prepareMessages(ctx context.Context, h *llm.StreamHandlers) []llm.Message {
	var view []llm.Message
	if a.Context == nil {
		view = a.Messages
	} else {
		a.Context.EnsureContextLimit(ctx)
		// Only compact at conversation boundaries (when the
		// most recent message is from the user).  Compacting
		// mid-tool-loop can drop assistant tool-call messages
		// whose results are still pending, confusing the LLM.
		if len(a.Messages) > 0 && a.Messages[len(a.Messages)-1].Role == "user" {
			if a.shouldCompactUsingCounts() {
				if h != nil && h.OnCompacting != nil {
					h.OnCompacting()
				}
				var pinned map[int]struct{}
				if a.PinManager != nil {
					pinned = a.PinManager.PinnedSet()
				}
				compacted, newPins, err := a.Context.CompactPinned(ctx, a.systemPromptPrefix(), a.Messages, pinned)
				if err == nil {
					// Publish the compacted history and invalidate the cached
					// token counts in one atomic step.
					a.replaceMessages(compacted)
					if a.PinManager != nil {
						a.PinManager.ReplacePins(newPins)
					}
					// lastTurnUsage is no longer representative after compaction.
					a.clearTurnUsage()
					a.resetSaveTracking()
				}
			}
		}
		// EnsureToolResultsCapped rewrites oversized tool bodies in place on
		// the live message array; exclude concurrent ContextStats clones.
		a.statsMu.Lock()
		if a.Context.EnsureToolResultsCapped(a.Messages) {
			// Tool bodies were rewritten in place, so cached counts for the
			// affected messages are stale. Drop the cache; it is rebuilt on
			// the next ContextStats/save.
			a.tokenCounts = nil
		}
		a.statsMu.Unlock()
		view = a.Messages
	}
	// Stabilize tool args on a.Messages (not view, which may be a copy) so
	// ArgsStabilized is persisted and we skip already-stable messages.
	a.stabilizeToolArgs()

	view = buildSystemView(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)

	a.recordViewForDrift(view)
	return view
}

// systemPromptPrefix returns the system/enrichment messages that precede
// canonical history on the wire (the view minus a.Messages). CompactPinned
// prepends these to the summarization request so the conversation prefix is
// byte-identical to the previous turn and provider prompt caching applies.
func (a *Agent) systemPromptPrefix() []llm.Message {
	if len(a.Messages) == 0 {
		return nil
	}
	view := buildSystemView(a.Messages, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)
	return view[:len(view)-len(a.Messages)]
}

// stabilizeToolArgs ensures every unstabilized tool call has its ArgsStr set.
// Skipped messages with ArgsStabilized=true — this turns an O(total_tool_calls)
// scan into O(new_tool_calls) per turn.
// It mutates Messages in place (ArgsStabilized, ToolCall ArgsStr), so it runs
// under statsMu to exclude concurrent clones (ContextStats/SnapshotMessages).
func (a *Agent) stabilizeToolArgs() {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	for i := range a.Messages {
		if a.Messages[i].ArgsStabilized {
			continue
		}
		for j := range a.Messages[i].ToolCalls {
			llm.StabilizeToolCallArgs(&a.Messages[i].ToolCalls[j])
		}
		a.Messages[i].ArgsStabilized = true
	}
}

// CompactHistory manually compacts conversation history at a task boundary.
func (a *Agent) CompactHistory(ctx context.Context) error {
	if a.Context == nil {
		return fmt.Errorf("context management is not configured")
	}
	if len(a.Messages) <= a.Context.Settings.CompactKeepRecentMessages+1 {
		return fmt.Errorf("not enough history to compact (%d messages)", len(a.Messages))
	}
	compacted, newPins, err := a.Context.CompactPinned(ctx, a.systemPromptPrefix(), a.Messages, pinnedSet(a.PinManager))
	if err != nil {
		return err
	}
	a.replaceMessages(compacted)
	if a.PinManager != nil {
		a.PinManager.ReplacePins(newPins)
	}
	// lastTurnUsage is no longer representative after compaction.
	a.clearTurnUsage()
	a.resetSaveTracking()
	return nil
}

func pinnedSet(p *PinManager) map[int]struct{} {
	if p == nil {
		return nil
	}
	return p.PinnedSet()
}

func formatToolError(result string, err error) string {
	if result == "" {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Error: %v\n\nOutput:\n%s", err, result)
}

func (a *Agent) appendToolResult(tc llm.ToolCall, result string) {
	if a.Context != nil {
		result = a.Context.TruncateToolResult(result)
	}
	a.appendMessage(llm.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: tc.ID,
		CreatedAt:  time.Now().Truncate(time.Millisecond),
	})
}

// StreamProcessInput streams tokens to the handlers as they arrive.
// It returns the final accumulated response or an error.
func (a *Agent) StreamProcessInput(ctx context.Context, input string, h *llm.StreamHandlers) (string, error) {
	return a.StreamProcessInputWithImages(ctx, input, nil, h)
}

// StreamProcessInputWithImages is StreamProcessInput with optional
// user-attached images (vision input). images is nil for text-only prompts.
func (a *Agent) StreamProcessInputWithImages(ctx context.Context, input string, images []llm.ImageInput, h *llm.StreamHandlers) (string, error) {
	a.appendMessage(llm.Message{
		Role:      "user",
		Content:   input,
		Images:    images,
		CreatedAt: time.Now().Truncate(time.Millisecond),
	})
	// If the session doesn't have a label yet, derive one from the first user message.
	if a.SessionLabelSnapshot() == "" {
		a.setSessionLabel(llm.SessionLabel(a.Messages))
	}
	// Persist immediately so a failed/cancelled turn does not drop the user message.
	a.FlushSession()

	if err := a.requireModelSelected(ctx); err != nil {
		a.truncateMessages(1)
		// The just-appended user message is being dropped: re-derive the
		// label from what remains, so a session whose only message failed
		// does not keep a stale title (its message no longer exists).
		a.setSessionLabel(llm.SessionLabel(a.Messages))
		a.resetSaveTracking()
		a.FlushSession()
		return "", err
	}

	if h == nil {
		h = &llm.StreamHandlers{}
	}
	for first := true; ; first = false {
		if ctx.Err() != nil {
			finishStreamUI(h)
			a.RepairOrphanToolCalls()
			a.FlushSession()
			return "", ctx.Err()
		}
		view := a.prepareMessages(ctx, h)

		if first && h.OnStart != nil {
			h.OnStart()
		} else if !first && h.OnRoundStart != nil {
			h.OnRoundStart()
		}
		if ctx.Err() != nil {
			finishStreamUI(h)
			a.RepairOrphanToolCalls()
			a.FlushSession()
			return "", ctx.Err()
		}

		a.Provider.SetThinkingLevel(string(a.ThinkingLevel))
		result, err := a.Provider.GenerateResponseStream(ctx, view, a.AllowedToolNames(), a.llmTools(), h)
		if err != nil {
			finishStreamUI(h)
			if ctx.Err() != nil {
				a.RepairOrphanToolCalls()
			}
			a.FlushSession()
			return "", err
		}
		a.recordTurnUsage(result.Usage)
		a.statsMu.Lock()
		a.UsageAccum.Add(result.Usage)
		a.statsMu.Unlock()

		if len(result.ToolCalls) == 0 {
			finishStreamUI(h)
			// A result with no content, no refusal, and no tool calls is a
			// truncated turn (e.g. finish_reason="length" after consuming the
			// output budget on reasoning). Persisting it would leave a ghost
			// assistant message that renders as an empty reply, pollutes later
			// turns, and becomes a fork point. Surface it as an error instead
			// and let the user retry.
			if result.Content == "" && result.Refusal == "" {
				return "", fmt.Errorf("model returned no output (response was truncated mid-reasoning); please try again")
			}
			a.appendMessage(llm.Message{
				Role:      "assistant",
				Content:   result.Content,
				Reasoning: result.Reasoning,
				Refusal:   result.Refusal,
				CreatedAt: time.Now().Truncate(time.Millisecond),
			})
			a.FlushSession()
			if result.Content != "" {
				return result.Content, nil
			}
			// Refusal is user-visible when the model declined without content.
			return result.Refusal, nil
		}

		if h.OnStreamEnd != nil {
			h.OnStreamEnd()
		}

		if result.PartialStream && h.OnRecoverPartialStream != nil {
			h.OnRecoverPartialStream()
		}

		ensureToolCallIDs(result.ToolCalls)
		for i := range result.ToolCalls {
			llm.StabilizeToolCallArgs(&result.ToolCalls[i])
		}

		a.appendMessage(llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			Reasoning: result.Reasoning,
			Refusal:   result.Refusal,
			ToolCalls: result.ToolCalls,
			CreatedAt: time.Now().Truncate(time.Millisecond),
		})

		// Read-only tool batches (every call parallel-safe and none shadowed
		// by an MCP tool) run concurrently, bounded by maxParallelTools;
		// anything else runs strictly sequentially in model order so mutating
		// tools stay serialized and results append in the model's call order.
		if a.toolCallsParallelEligible(result.ToolCalls) {
			if a.executeToolCallsParallel(ctx, h, result.ToolCalls) {
				finishStreamUI(h)
				a.FlushSession()
				return "", ctx.Err()
			}
		} else {
			for i, tc := range result.ToolCalls {
				if ctx.Err() != nil {
					// Preserve a valid tool-call/result protocol for the next turn.
					a.appendCanceledToolResults(result.ToolCalls[i:])
					finishStreamUI(h)
					a.FlushSession()
					return "", ctx.Err()
				}
				if h.OnToolCall != nil {
					h.OnToolCall(tc)
				}
				if h.OnToolExecute != nil {
					h.OnToolExecute(tc.Name)
				}

				// Attach the live-output sink (if any) to this tool call's
				// context so shell tools can stream chunks to the UI as the
				// command runs. The sink is per-tool so each command gets its
				// own identity (id/name) in the handler.
				toolCtx := ctx
				if h.OnToolOutput != nil {
					toolCtx = ContextWithToolOutput(ctx, func(command, chunk string) {
						h.OnToolOutput(tc.ID, tc.Name, command, chunk)
					})
				}
				res, errTool := a.executeTool(toolCtx, tc)
				if ctx.Err() != nil {
					if errTool == nil {
						res = "The operation was cancelled by the user."
					} else if errTool == context.Canceled {
						res = "The operation was cancelled by the user."
					} else {
						res = formatToolError(res, errTool)
					}
					if h.OnToolResult != nil {
						h.OnToolResult(tc.ID, tc.Name, res, false)
					}
					a.appendToolResult(tc, res)
					a.appendCanceledToolResults(result.ToolCalls[i+1:])
					finishStreamUI(h)
					a.FlushSession()
					return "", ctx.Err()
				}
				success := errTool == nil
				if errTool != nil {
					res = formatToolError(res, errTool)
				}

				if h.OnToolResult != nil {
					h.OnToolResult(tc.ID, tc.Name, res, success)
				}

				a.appendToolResult(tc, res)
			}
		}
		a.persistSession()
	}
}

func (a *Agent) appendCanceledToolResults(toolCalls []llm.ToolCall) {
	const msg = "Tool execution was skipped because the user cancelled the operation."
	for _, tc := range toolCalls {
		a.appendToolResult(tc, msg)
	}
}

// toolCallsParallelEligible reports whether every tool call in the batch can
// run concurrently within the turn: all are builtin read-only tools and none
// is shadowed by an MCP tool of the same name (MCP side effects are unknown,
// so MCP tools always stay sequential).
func (a *Agent) toolCallsParallelEligible(toolCalls []llm.ToolCall) bool {
	if len(toolCalls) < 2 {
		return false
	}
	if a.MCPRegistry != nil {
		if names := a.MCPRegistry.ToolNames(); names != nil {
			for _, tc := range toolCalls {
				if _, ok := names[tc.Name]; ok {
					return false
				}
			}
		}
	}
	for _, tc := range toolCalls {
		if !parallelSafeTools[tc.Name] {
			return false
		}
	}
	return true
}

// executeToolCallsParallel runs every tool call concurrently, bounded by
// maxParallelTools, then fires OnToolResult callbacks and appends results in
// model order so the tool-call/result protocol stays valid (every tool_call
// gets a matching tool result). It returns true when the turn was cancelled
// during execution; in that case completed results are preserved and
// interrupted calls read as cancelled.
func (a *Agent) executeToolCallsParallel(ctx context.Context, h *llm.StreamHandlers, toolCalls []llm.ToolCall) bool {
	type execResult struct {
		res string
		err error
	}
	results := make([]execResult, len(toolCalls))

	if ctx.Err() != nil {
		return true
	}
	for i := range toolCalls {
		tc := toolCalls[i]
		if h.OnToolCall != nil {
			h.OnToolCall(tc)
		}
		if h.OnToolExecute != nil {
			h.OnToolExecute(tc.Name)
		}
	}

	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i := range toolCalls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = execResult{err: context.Canceled}
				return
			}
			defer func() { <-sem }()
			res, err := a.executeTool(ctx, toolCalls[i])
			results[i] = execResult{res: res, err: err}
		}(i)
	}
	wg.Wait()

	cancelled := ctx.Err() != nil
	for i, tc := range toolCalls {
		res, errTool := results[i].res, results[i].err
		if cancelled {
			if errTool == nil {
				res = "The operation was cancelled by the user."
			} else if errors.Is(errTool, context.Canceled) {
				res = "The operation was cancelled by the user."
			} else {
				res = formatToolError(res, errTool)
			}
			if h.OnToolResult != nil {
				h.OnToolResult(tc.ID, tc.Name, res, false)
			}
			a.appendToolResult(tc, res)
			continue
		}
		success := errTool == nil
		if errTool != nil {
			res = formatToolError(res, errTool)
		}
		if h.OnToolResult != nil {
			h.OnToolResult(tc.ID, tc.Name, res, success)
		}
		a.appendToolResult(tc, res)
	}
	return cancelled
}

// RepairOrphanToolCalls appends cancelled tool-result placeholders for any
// trailing assistant tool_calls that lack matching tool messages. Call on
// cancel/shutdown so the next turn keeps a valid tool-call/result protocol.
// Returns true when messages were modified.
//
// The scan is a single backward pass: seen tracks the tool-result IDs in the
// contiguous run of tool messages immediately after the current position
// (matching the previous per-message forward scan, which broke at the first
// non-tool message). Any other message resets the run.
func (a *Agent) RepairOrphanToolCalls() bool {
	if a == nil || len(a.Messages) == 0 {
		return false
	}
	modified := false
	seen := make(map[string]struct{})
	for i := len(a.Messages) - 1; i >= 0; i-- {
		msg := a.Messages[i]
		switch {
		case msg.Role == "tool":
			if id := msg.ToolCallID; id != "" {
				seen[id] = struct{}{}
			}
		case msg.Role == "assistant" && len(msg.ToolCalls) > 0:
			var missing []llm.ToolCall
			for _, tc := range msg.ToolCalls {
				if _, ok := seen[tc.ID]; !ok {
					missing = append(missing, tc)
				}
			}
			if len(missing) > 0 {
				a.appendCanceledToolResults(missing)
				modified = true
			}
			// This assistant message is itself a non-tool message, so the
			// tool-result run it just consumed does not apply to any earlier
			// assistant tool-call message (the old forward scan broke on it).
			seen = make(map[string]struct{})
		default:
			seen = make(map[string]struct{})
		}
	}
	return modified
}

func stringArg(args map[string]interface{}, key string) (string, error) {
	val, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func stringArgOptional(args map[string]interface{}, key string) (string, error) {
	val, ok := args[key]
	if !ok {
		return "", nil
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func (a *Agent) toolContext(ctx context.Context) context.Context {
	if a.Executor != nil && !a.Executor.RequireDeleteApproval {
		ctx = ContextWithDeleteApprovalRequired(ctx, false)
	}
	return ctx
}

// boolArg reads an optional boolean tool argument, returning def when the
// key is absent.
func boolArg(args map[string]interface{}, key string, def bool) (bool, error) {
	val, ok := args[key]
	if !ok {
		return def, nil
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("argument %q must be a boolean", key)
	}
	return b, nil
}

func intArgOptional(args map[string]interface{}, key string) (int, error) {
	val, ok := args[key]
	if !ok {
		return 0, nil
	}
	switch v := val.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("argument %q must be an integer", key)
	}
}

func stringSliceArg(args map[string]interface{}, key string) ([]string, error) {
	val, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("missing required argument %q", key)
	}
	return coerceStringSlice(key, val)
}

func stringSliceArgOptional(args map[string]interface{}, key string) ([]string, error) {
	val, ok := args[key]
	if !ok {
		return nil, nil
	}
	return coerceStringSlice(key, val)
}

func coerceStringSlice(key string, val interface{}) ([]string, error) {
	switch v := val.(type) {
	case []string:
		return v, nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("argument %q[%d] must be a string", key, i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("argument %q must be an array of strings", key)
	}
}
