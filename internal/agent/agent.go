package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/projectfile"
	"gogen/internal/skills"
)

// persistMinInterval is the minimum time between debounced session writes.
// Final boundaries (turn complete, errors) bypass this via flushSession().
const persistMinInterval = 5 * time.Second

type Agent struct {
	Provider llm.LLMProvider
	Executor *Executor

	// Conversation state
	Context *contextmgr.Manager
	// Messages is owned by the turn goroutine for writes (append/compact/
	// restore); lock-free readers (ContextStats, SnapshotMessages) read it
	// under statsMu. See the statsMu comment below.
	Messages []llm.Message
	// PinManager is internally synchronized (methods take their own mutex);
	// the reference itself is set at construction and never reassigned.
	PinManager *PinManager
	// TodoManager is internally synchronized (Snapshot/AddTodo/... take the
	// manager's own mutex); the reference is set at construction and never
	// reassigned. The web fork path reads it without the session turnMu on
	// purpose (snapshot is lock-free w.r.t. turnMu — see sessionFork).
	TodoManager *TodoManager

	// Session persistence
	SessionStore SessionPersister
	// SessionID is guarded by the session turnMu: TUI runs on a single owner
	// goroutine, and web lifecycle ops (new/resume/fork/delete) write it
	// under the pane's turnMu or at construction (main.go startup restore,
	// NewSessionAgent). Do not read it for a live session without holding
	// turnMu.
	SessionID string
	// SessionLabel is guarded by statsMu (setSessionLabel /
	// SessionLabelSnapshot): web probes read it without turnMu while the
	// turn goroutine may derive it from the first user message.
	SessionLabel string
	// labelRenamed is true when SessionLabel was set deliberately
	// (RenameSession / session_rename tool). Guarded by statsMu; persisted
	// via SessionSnapshot.LabelRenamed so the store never regenerates the
	// label from the conversation (see sessionLabel).
	labelRenamed   bool
	SessionOneshot bool // true if this session was created by a single-prompt (-p) invocation
	// UsageAccum is guarded by statsMu (clearTurnUsage / snapshot reads).
	UsageAccum UsageAccumulator

	// OnModelChanged, when non-nil, is invoked after ValidateRestoredModel
	// processes a restored model so hosts can refresh their UI (the model
	// may have been cleared or replaced by sole-model auto-select). Set at
	// construction by the TUI/web hosts before the validation goroutine is
	// spawned; called from that goroutine. Must not block.
	OnModelChanged func()

	// modelUnverified is true when the selected model came from a session
	// restore or an external adoption (AdoptModel — the web /new pane-model
	// inheritance) and has not yet been confirmed to exist at the provider.
	// RestoreSessionLocal and AdoptModel set it; ValidateRestoredModel
	// clears it once the model is resolved (present, absent, or
	// auto-selected); SelectModel clears it (a selection comes from the
	// provider's own list). requireModelSelected re-checks a
	// still-unverified model on the first turn so a stale model from a
	// previous provider is never sent to the endpoint. Guarded by statsMu.
	modelUnverified bool

	// bgMu guards bgJobs: shell commands started with execute_command
	// background=true. Jobs outlive the turn that started them (they are
	// owned by the session, not the turn) and are killed when the session
	// closes (Close) or individually via background_job (action=cancel).
	bgMu   sync.Mutex
	bgJobs map[string]*BackgroundJob
	// bgRetain is how long a finished background job stays registered for
	// status polling before the reaper removes it (0 = the
	// defaultBackgroundJobRetain window). Read only from the job's wait
	// goroutine; tests set it before starting jobs.
	bgRetain time.Duration
	// bgMaxFinished caps how many finished jobs stay registered at once
	// (0 = defaultMaxFinishedBackgroundJobs); when a job finishes over the
	// cap, the oldest finished jobs are reaped immediately.
	bgMaxFinished int

	// sessionImages counts read_image attachments in this session. Atomic:
	// read_image handlers run concurrently in parallel read-only batches,
	// so the claim (reserveImageSlot) is a CAS loop. Enforces the soft
	// per-session cap (maxSessionImages) on image bytes — the most
	// expensive content in context, resent on every API request.
	sessionImages atomic.Int32

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

	// Auto-compaction failure backoff (guarded by statsMu): doubles per
	// failure (30s → 10min cap), reset on success.
	compactBackoffUntil time.Time
	compactBackoffDelay time.Duration

	// lastEmergencyFailCount is the message count at which the last
	// emergency-tier compaction failed (guarded by statsMu). Emergency
	// attempts are suppressed while the count has not grown past it (the
	// progress guard against a hot loop of identical failures); reset on
	// any successful compaction.
	lastEmergencyFailCount int

	// lastLastResortFailCount is the message count at which the last
	// last-resort condensation (Phase 0e) failed (guarded by statsMu).
	// Same progress-guard semantics as lastEmergencyFailCount: a retry at
	// the unchanged count would hit the same failure, so attempts are
	// suppressed until the conversation grows past that count; reset on
	// success.
	lastLastResortFailCount int

	// overheadMu guards overheadFingerprint/overheadTokens: the cached wire
	// overhead (system prompt + tool definitions) in tokens. Recomputed only
	// when the content fingerprint of (system prompt, tool definitions)
	// changes, so the per-round prepareMessages call never re-tokenizes the
	// tool set. Deliberately not statsMu: tokenizing is CPU-heavy and the
	// cache is consulted only on the turn goroutine (shouldCompactUsingCounts),
	// not by lock-free readers.
	overheadMu          sync.Mutex
	overheadFingerprint string
	overheadTokens      int

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

	// Feature flags, live-toggleable from the web settings modal (config WS
	// message). The atomic stores publish the toggle to every concurrent
	// reader: llmTools/AllowedToolNames consult BoardEnabled/SubagentsEnabled
	// per turn, and the toggle handler rebuilds toolHandlers under toolMu so
	// executeTool's map lookup is the gate. subagentMaxDepth bounds nesting
	// (main agent = depth 0; default 1 = subagents cannot spawn subagents)
	// and subagentMaxConcurrent bounds per-parent concurrent subagents
	// (default config.DefaultSubagentMaxConcurrent).
	boardEnabled          atomic.Bool
	subagentsEnabled      atomic.Bool
	subagentMaxDepth      atomic.Int32
	subagentMaxConcurrent atomic.Int32

	// boardManager is the shared project board (one instance per workspace
	// in web mode, per agent in TUI/CLI). Set when the board feature is
	// enabled; the nil-guard mirrors the MCP registry contract.
	boardManager atomic.Pointer[BoardManager]

	// skillsEnabled mirrors the config skills flag; skillsManager is the
	// shared skill discovery manager (one per workspace in web mode, per
	// agent in TUI/CLI). Both follow the board pattern: the tool is exposed
	// only when the flag is on AND the manager is installed.
	skillsEnabled atomic.Bool
	skillsManager atomic.Pointer[skills.Manager]

	// instructionsEnabled mirrors the config agent_instructions flag
	// (config-only, set at construction). workspaceInstructions is the
	// rendered AGENTS.md/CLAUDE.md section for the CURRENT working dir,
	// refreshed on every working-dir change so the system prompt never
	// carries a stale project's instructions.
	instructionsEnabled   atomic.Bool
	instructionsMu        sync.RWMutex
	workspaceInstructions string

	// spawnerMu guards spawner (the nested-session runner installed once at
	// startup by the server/TUI). Reads happen per turn (llmTools,
	// executeTool), so the guard keeps the write/read race-free.
	spawnerMu sync.RWMutex
	spawner   SubagentSpawner
	// subagentDepth is this agent's nesting depth (main agent = 0; the
	// spawner sets depth+1 on children). Bounds spawning against
	// SubagentMaxDepth (default 1 = subagents cannot spawn subagents).
	subagentDepth atomic.Int32
	// parentID is non-empty for nested (subagent) sessions: persisted into
	// the session snapshot so the store can exclude them from the flat
	// list, cascade-delete them with the parent, and cap them per parent.
	parentID atomic.Value // string
	// subagentOutcome records the final outcome of a nested (subagent)
	// session (nil for top-level sessions and children that have not
	// finished). Persisted into the snapshot so the sidebar can render the
	// true result after a reload/restart, when the subagent_started/
	// finished events are not replayed.
	subagentOutcome atomic.Value // *subagentOutcome

	// boardHookMu guards boardHook: the web server installs it so agent
	// board mutations broadcast a fresh board_state AND a success notice
	// (toast) to every client — the user sees agent-triggered board
	// changes even when they didn't initiate them. The callback receives
	// the mutation's output message ("Moved board item #1 to in_progress…").
	// Nil in TUI/CLI.
	boardHookMu sync.RWMutex
	boardHook   func(msg string)

	// jobNoticeHookMu guards jobNoticeHook: hosts install it so a
	// background job finishing naturally delivers a notice into the
	// session (job_notices feature). The callback receives a one-line
	// summary ("[job] job-xxx (command) exited with code N"). Nil when the
	// feature is off.
	jobNoticeHookMu sync.RWMutex
	jobNoticeHook   func(summary string)

	// reportHookMu guards reportHook: the web spawner installs it on
	// continuable children so the child-scoped report tool can deliver a
	// progress message into the live parent session. Nil for non-children
	// and TUI children (the report tool is gated on it).
	reportHookMu sync.RWMutex
	reportHook   func(text string) error

	// patchFailStreak counts consecutive patch_file failures so the agent loop
	// can steer the model away from retrying the same stale diff indefinitely.
	patchFailStreak atomic.Int32

	// patchTurnStrikes counts consecutive patch_file mismatch failures within
	// a single turn so the agent loop can hard-stop a model stuck in a patch
	// retry loop. Reset at the start of every turn; reaching the limit aborts
	// the turn (runToolRound returns toolRoundStopped), unlike
	// patchFailStreak which only decorates the error with advice.
	patchTurnStrikes atomic.Int32

	// patchStrikeKey remembers the failure report of the last patch_file
	// mismatch so patchTurnStrikes only accumulates while the SAME diff keeps
	// failing (same target, same mismatch). A model iterating across
	// different files or diffs is making progress and must not be stopped.
	patchStrikeKey atomic.Value // string

	// overflowRetries counts the provider context-window refusals already
	// recovered in the current turn (Phase 3, handleOverflowError). Reset
	// at the start of every turn; capped at 1 — a second refusal in the
	// same turn (or a failed forced compaction) returns the actionable
	// terminal error instead of retrying again.
	overflowRetries atomic.Int32

	// ghostRetries counts CONSECUTIVE truncated-turn (ghost) rounds — a
	// stream that ends with reasoning but no content, refusal, or tool
	// calls — since the last successful round. Providers emit this
	// transiently (e.g. a finish_reason="stop" right after
	// reasoning-only chunks), so the round is retried once in-loop instead
	// of failing the turn. Any successful round (content, refusal, or tool
	// calls) resets the counter, so a long multi-round turn gets a fresh
	// retry after each recovery; the cap only stops a model that ghosts
	// back-to-back — a second consecutive ghost round surfaces the
	// "model returned no output" error to the user.
	ghostRetries atomic.Int32
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
	// subagentStatus/subagentSummary mirror the recorded subagent outcome:
	// writing it right after a turn's full save must force ANOTHER full
	// save (deltas do not carry the outcome to the index).
	subagentStatus  string
	subagentSummary string
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

// SetBoardEnabled toggles the project kanban board feature for this agent.
// The web server publishes live toggles to every session agent; per-turn
// tool derivation (llmTools/AllowedToolNames) reads the value atomically.
func (a *Agent) SetBoardEnabled(on bool) {
	a.boardEnabled.Store(on)
}

// BoardEnabled reports whether the board tool is registered for this agent.
func (a *Agent) BoardEnabled() bool {
	return a.boardEnabled.Load()
}

// SetSubagentsEnabled toggles the subagent tool for this agent (see
// SetBoardEnabled for the live-toggle contract).
func (a *Agent) SetSubagentsEnabled(on bool) {
	a.subagentsEnabled.Store(on)
}

// SubagentsEnabled reports whether the subagent tool is registered for this
// agent.
func (a *Agent) SubagentsEnabled() bool {
	return a.subagentsEnabled.Load()
}

// SetSkillsEnabled toggles the skill tool for this agent (see
// SetBoardEnabled for the live-toggle contract; skills is config-only in
// v1, so this is set at construction).
func (a *Agent) SetSkillsEnabled(on bool) {
	a.skillsEnabled.Store(on)
}

// SkillsEnabled reports whether the skill tool is registered for this agent.
func (a *Agent) SkillsEnabled() bool {
	return a.skillsEnabled.Load()
}

// SetSubagentMaxDepth sets the maximum subagent nesting depth (main agent =
// depth 0). Values <= 0 fall back to the config default.
func (a *Agent) SetSubagentMaxDepth(depth int) {
	a.subagentMaxDepth.Store(int32(depth))
}

// SubagentMaxDepth returns the effective maximum subagent nesting depth.
func (a *Agent) SubagentMaxDepth() int {
	if d := int(a.subagentMaxDepth.Load()); d > 0 {
		return d
	}
	return config.DefaultSubagentMaxDepth
}

// SetSubagentMaxConcurrent sets the maximum number of subagents that may
// run concurrently per parent session. Values <= 0 fall back to the config
// default.
func (a *Agent) SetSubagentMaxConcurrent(n int) {
	a.subagentMaxConcurrent.Store(int32(n))
}

// SubagentMaxConcurrent returns the effective per-parent concurrent-subagent
// limit.
func (a *Agent) SubagentMaxConcurrent() int {
	if n := int(a.subagentMaxConcurrent.Load()); n > 0 {
		return n
	}
	return config.DefaultSubagentMaxConcurrent
}

// SetBoardManager attaches the shared project board manager. The web server
// sets the same manager on every session agent (so claims serialize);
// TUI/CLI sets a per-agent manager. nil detaches.
func (a *Agent) SetBoardManager(m *BoardManager) {
	a.boardManager.Store(m)
}

// BoardManager returns the attached board manager (nil when the board
// feature is disabled or not wired).
func (a *Agent) BoardManager() *BoardManager {
	return a.boardManager.Load()
}

// SetSkillsManager attaches the shared skill discovery manager (nil
// detaches). The web server sets the same manager on every session agent;
// TUI/CLI sets a per-agent manager. The skill tool stays gated on
// SkillsEnabled regardless.
func (a *Agent) SetSkillsManager(m *skills.Manager) {
	a.skillsManager.Store(m)
}

// SkillsManager returns the attached skill manager (nil when skills are
// disabled or not wired).
func (a *Agent) SkillsManager() *skills.Manager {
	return a.skillsManager.Load()
}

// SetInstructionsEnabled toggles AGENTS.md/CLAUDE.md loading for this
// agent (config-only in v1; set at construction).
func (a *Agent) SetInstructionsEnabled(on bool) {
	a.instructionsEnabled.Store(on)
}

// InstructionsEnabled reports whether AGENTS.md/CLAUDE.md loading is on.
func (a *Agent) InstructionsEnabled() bool {
	return a.instructionsEnabled.Load()
}

// RefreshWorkspaceInstructions re-renders the AGENTS.md/CLAUDE.md section
// from dir into workspaceInstructions. Called at construction and after
// every working-dir change; no-op when the feature is off. Discovery
// skips missing roots and unreadable files (never an error).
func (a *Agent) RefreshWorkspaceInstructions(dir string) {
	if !a.instructionsEnabled.Load() {
		return
	}
	instr, err := projectfile.LoadInstructions(dir)
	if err != nil {
		log.Printf("warning: agent_instructions: %v", err)
		return
	}
	a.instructionsMu.Lock()
	a.workspaceInstructions = instr
	a.instructionsMu.Unlock()
}

// EffectiveGuidelines returns the project guidelines with the workspace
// instruction section appended below, rendered from the CURRENT working
// dir. Thread-safe; used by the view builders (buildSystemView /
// buildSystemSuffix).
func (a *Agent) EffectiveGuidelines() string {
	if !a.instructionsEnabled.Load() {
		return a.ProjectGuidelines
	}
	a.instructionsMu.RLock()
	instr := a.workspaceInstructions
	a.instructionsMu.RUnlock()
	if instr == "" {
		return a.ProjectGuidelines
	}
	if a.ProjectGuidelines == "" {
		return instr
	}
	return a.ProjectGuidelines + "\n\n" + instr
}

// SetSubagentSpawner installs the nested-session runner (nil detaches).
// Called once at startup by the web server and TUI runner; the subagent tool
// stays gated on SubagentsEnabled regardless.
func (a *Agent) SetSubagentSpawner(s SubagentSpawner) {
	a.spawnerMu.Lock()
	a.spawner = s
	a.spawnerMu.Unlock()
}

// SubagentSpawner returns the installed nested-session runner (nil when
// subagents are unavailable in this mode).
func (a *Agent) SubagentSpawner() SubagentSpawner {
	a.spawnerMu.RLock()
	defer a.spawnerMu.RUnlock()
	return a.spawner
}

// SetSubagentDepth sets this agent's nesting depth (0 = main agent). The
// spawner sets depth+1 on children it creates.
func (a *Agent) SetSubagentDepth(depth int) {
	a.subagentDepth.Store(int32(depth))
}

// SubagentDepth returns this agent's nesting depth (0 = main agent).
func (a *Agent) SubagentDepth() int {
	return int(a.subagentDepth.Load())
}

// SetParentID marks this session as a nested (subagent) child of parentID
// (empty clears the mark).
func (a *Agent) SetParentID(parentID string) {
	a.parentID.Store(parentID)
}

// ParentID returns the parent session id for nested (subagent) sessions
// (empty for top-level sessions).
func (a *Agent) ParentID() string {
	v, _ := a.parentID.Load().(string)
	return v
}

// subagentOutcome is the persisted final outcome of a nested (subagent)
// session. Status is "" (unknown / not finished), "success", or "failed".
type subagentOutcome struct {
	Status  string
	Summary string
}

// SetSubagentOutcome records the final outcome of a nested (subagent)
// session. The spawner writes it at every terminal transition (natural
// completion, cancellation, eviction) so the persisted snapshot carries the
// truth across restarts; the sidebar reads it from the sessions payload
// when the live subagent_started/finished events are gone. Empty status
// means the child has not finished (or is not a subagent).
func (a *Agent) SetSubagentOutcome(status, summary string) {
	a.subagentOutcome.Store(&subagentOutcome{Status: status, Summary: summary})
}

// SubagentOutcome returns the recorded final outcome: the status ("" /
// "success" / "failed") and the summary text.
func (a *Agent) SubagentOutcome() (string, string) {
	v := a.subagentOutcome.Load()
	if v == nil {
		return "", ""
	}
	o := v.(*subagentOutcome)
	return o.Status, o.Summary
}

// SetOnBoardChanged installs a callback invoked after every successful board
// mutation made through this agent's board tool; it receives the mutation's
// output message. The web server uses it to broadcast a fresh board_state and
// a success notice (toast) to all clients. nil detaches.
func (a *Agent) SetOnBoardChanged(h func(msg string)) {
	a.boardHookMu.Lock()
	a.boardHook = h
	a.boardHookMu.Unlock()
}

// notifyBoardChanged fires the installed board-change hook with the
// mutation's output message, if any hook is installed.
func (a *Agent) notifyBoardChanged(msg string) {
	a.boardHookMu.RLock()
	h := a.boardHook
	a.boardHookMu.RUnlock()
	if h != nil {
		h(msg)
	}
}

// SetJobNoticeHook installs a callback invoked when a background job
// (execute_command background=true) finishes naturally; it receives a
// one-line summary. Hosts wire it to the message-delivery service so the
// session is notified without polling. nil detaches.
func (a *Agent) SetJobNoticeHook(h func(summary string)) {
	a.jobNoticeHookMu.Lock()
	a.jobNoticeHook = h
	a.jobNoticeHookMu.Unlock()
}

// fireJobNotice invokes the installed job-notice hook, if any.
func (a *Agent) fireJobNotice(summary string) {
	a.jobNoticeHookMu.RLock()
	h := a.jobNoticeHook
	a.jobNoticeHookMu.RUnlock()
	if h != nil {
		h(summary)
	}
}

// SetReportHook installs the child-scoped report hook (continuable
// subagents only): delivering a progress message to the live parent
// session. nil detaches (the report tool is then not registered).
func (a *Agent) SetReportHook(h func(text string) error) {
	a.reportHookMu.Lock()
	a.reportHook = h
	a.reportHookMu.Unlock()
}

// ReportHook returns the installed report hook (nil when this agent is not
// a continuable child).
func (a *Agent) ReportHook() func(text string) error {
	a.reportHookMu.RLock()
	defer a.reportHookMu.RUnlock()
	return a.reportHook
}

// reportToParent delivers a progress message to the live parent session via
// the installed hook.
func (a *Agent) reportToParent(text string) error {
	a.reportHookMu.RLock()
	h := a.reportHook
	a.reportHookMu.RUnlock()
	if h == nil {
		return fmt.Errorf("report is not available in this session")
	}
	return h(text)
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
// Manager.ShouldCompactWithOverhead (a full EstimateTokens pass) when the
// cache is empty or incomplete (e.g. right after a compaction or session
// restore).
//
// Wire overhead accounting: the per-message counts cover the canonical
// messages only — the system prompt and tool definitions (10-30k tokens)
// ride on every request but are not in a.Messages. When the provider
// prompt_tokens baseline is fresh, it already includes that overhead, so it
// must NOT be added again (double-count trap). When the baseline is absent
// (post-compaction, post-restore, first turn), the wire overhead is added so
// the trigger fires on time instead of tens of thousands of tokens late.
func (a *Agent) shouldCompactUsingCounts() bool {
	if a.Context == nil {
		return false
	}
	a.statsMu.RLock()
	counts := a.tokenCounts
	msgs := a.Messages
	complete := counts != nil && len(counts) == len(msgs)
	baselineTokens, baselineMsgCount := a.apiBaselinePromptTokens, a.apiBaselineMsgCount
	a.statsMu.RUnlock()
	// Fresh when the provider's prompt_tokens were recorded for a list no
	// larger than the current one (the list only grew since that request).
	// A shrunken list (rollback) leaves the baseline stale — the API count
	// would over-report, so the local estimate is used instead.
	baselineFresh := baselineTokens > 0 && baselineMsgCount > 0 && baselineMsgCount <= len(msgs)
	if !complete {
		overhead := 0
		if !baselineFresh {
			overhead = a.wireOverheadTokens()
		}
		return a.Context.ShouldCompactWithOverhead(msgs, overhead)
	}
	if !a.Context.AutoCompactEnabled() {
		return false
	}
	if len(msgs) <= a.Context.CompactKeepRecentMessages()+1 {
		return false
	}
	return a.compactionTokenTotal() >= a.Context.CompactBudget()
}

// compactionTokenTotal returns the estimated total token count of the
// conversation (system prompt + tool definitions + canonical messages),
// using the same accounting as shouldCompactUsingCounts: the provider's
// exact prompt_tokens baseline when fresh, otherwise the cached per-message
// counts plus the wire overhead. Returns -1 when the per-message count
// cache is incomplete (e.g. right after a session restore) so callers can
// fall back to the full-estimate path.
func (a *Agent) compactionTokenTotal() int {
	a.statsMu.RLock()
	counts := a.tokenCounts
	msgs := a.Messages
	complete := counts != nil && len(counts) == len(msgs)
	baselineTokens, baselineMsgCount := a.apiBaselinePromptTokens, a.apiBaselineMsgCount
	a.statsMu.RUnlock()
	if !complete {
		return -1
	}
	// Fresh when the provider's prompt_tokens were recorded for a list no
	// larger than the current one (the list only grew since that request).
	baselineFresh := baselineTokens > 0 && baselineMsgCount > 0 && baselineMsgCount <= len(msgs)
	total := 0
	for _, c := range counts {
		total += c
	}
	// Use the provider's exact prompt_tokens for messages in the last request
	// (cl100k misjudges other tokenizers); estimate only the suffix appended
	// since. The provider count already includes the system prompt and tool
	// definitions, so the wire overhead is NOT added again (double-count
	// trap). Cleared by clearTurnUsage after a compaction.
	if baselineFresh {
		local := 0
		for _, c := range counts[baselineMsgCount:] {
			local += c
		}
		total = baselineTokens + local
	} else {
		// Baseline absent (post-compaction, post-restore, first turn): the
		// local counts cover the canonical messages only, so add the wire
		// overhead (system prompt + tool definitions) they omit.
		total += a.wireOverheadTokens()
	}
	return total
}

// wireOverheadTokens returns the estimated wire token cost of everything a
// provider request carries besides the canonical messages: the system prompt
// (or only its enrichment suffix when the history already carries a system
// message, whose base content is in the per-message counts) plus all tool
// definitions. The result is cached and recomputed only when the content
// fingerprint of (system prompt, tool definitions) changes, so the
// per-round prepareMessages call does not re-tokenize the tool set.
func (a *Agent) wireOverheadTokens() int {
	var sysContent string
	if prefix := a.systemPromptPrefix(); prefix != nil {
		sysContent = prefix[0].Content
	} else {
		// History carries a system message: only the enrichment suffix is
		// wire overhead (the base content is counted in the messages).
		sysContent = buildSystemSuffix(a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode)
	}
	tools := a.llmTools()
	fp := overheadFingerprint(sysContent, tools)
	a.overheadMu.Lock()
	if fp != a.overheadFingerprint {
		tokens := contextmgr.EstimateToolTokens(tools)
		if sysContent != "" && a.Context != nil {
			tokens += a.Context.EstimateTokens([]llm.Message{{Role: "system", Content: sysContent}})
		}
		a.overheadFingerprint = fp
		a.overheadTokens = tokens
	}
	t := a.overheadTokens
	a.overheadMu.Unlock()
	return t
}

// overheadFingerprint builds a content fingerprint of (system prompt, tool
// definitions) for the wire-overhead cache: any change to the system prompt
// text or any tool definition produces a different fingerprint, invalidating
// the cached token estimate. Length-prefixed segments make the hash
// collision-resistant against concatenation ambiguity.
func overheadFingerprint(sysContent string, tools []llm.Tool) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00", len(sysContent))
	h.Write([]byte(sysContent))
	for _, t := range tools {
		s := contextmgr.ToolDefinitionString(t)
		fmt.Fprintf(h, "%d\x00", len(s))
		h.Write([]byte(s))
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// compactAttemptDue reports whether the auto-compaction failure backoff has expired.
func (a *Agent) compactAttemptDue() bool {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return time.Now().After(a.compactBackoffUntil)
}

// emergencySummaryAllowance is the token budget assumed for the summary
// output when sizing the emergency compaction middle: the middle must be
// large enough to save (total - hardLimit) PLUS this much, so the summary
// itself still lands the conversation under the window.
const emergencySummaryAllowance = 2000

// emergencyCompactDue reports whether the EMERGENCY compaction tier should
// fire: the conversation total has reached the hard window
// (ContextLimit - CompactReserveTokens, where the provider would refuse the
// request) and the progress guard allows a new attempt. Unlike the normal
// tier it ignores the failure backoff — a provider refusal is worse than a
// redundant summarization call — but it refuses to retry a compaction that
// already failed at a message count >= the current one
// (lastEmergencyFailCount), so a permanently broken summarization path
// cannot hot-loop an expensive failure on every turn. It also requires at
// least 3 messages: with fewer, there is nothing between the starting
// prompt and the current one to summarize. total < 0 (incomplete count
// cache) never fires: the normal tier's full-estimate fallback covers that
// state.
func (a *Agent) emergencyCompactDue(total int) bool {
	if a.Context == nil || total < 0 {
		return false
	}
	hardLimit := a.Context.HardLimit()
	if hardLimit <= 0 || total < hardLimit {
		return false
	}
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	if len(a.Messages) < 3 {
		return false
	}
	return a.lastEmergencyFailCount < len(a.Messages)
}

// noteEmergencyCompactFailure records an emergency-tier compaction failure
// at the current message count: the progress guard suppresses further
// emergency attempts until the conversation grows past that count (a retry
// at the same count would hit the same failure).
func (a *Agent) noteEmergencyCompactFailure() {
	a.statsMu.Lock()
	if n := len(a.Messages); n > a.lastEmergencyFailCount {
		a.lastEmergencyFailCount = n
	}
	a.statsMu.Unlock()
}

// noteCompactFailure records a failed auto-compaction and backs off the next attempt exponentially.
func (a *Agent) noteCompactFailure(err error) {
	log.Printf("auto-compaction failed (backing off): %v", err)
	a.statsMu.Lock()
	if a.compactBackoffDelay == 0 {
		a.compactBackoffDelay = 30 * time.Second
	} else {
		a.compactBackoffDelay *= 2
		if a.compactBackoffDelay > 10*time.Minute {
			a.compactBackoffDelay = 10 * time.Minute
		}
	}
	a.compactBackoffUntil = time.Now().Add(a.compactBackoffDelay)
	a.statsMu.Unlock()
}

// noteCompactSuccess resets the auto-compaction failure backoff and the
// emergency-tier progress guard.
func (a *Agent) noteCompactSuccess() {
	a.statsMu.Lock()
	a.compactBackoffUntil = time.Time{}
	a.compactBackoffDelay = 0
	a.lastEmergencyFailCount = 0
	a.statsMu.Unlock()
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
	if bm := a.BoardManager(); bm != nil {
		// Global mode keeps the global board; project mode re-points to the
		// new working dir's .gogen/board (D3).
		bm.SetWorkingDir(dir)
	}
	if sm := a.SkillsManager(); sm != nil {
		// Re-point the project skills root to the new working dir (the user
		// root is unchanged).
		sm.SetWorkingDir(dir)
	}
	if a.instructionsEnabled.Load() {
		// Re-render the AGENTS.md/CLAUDE.md section for the new dir: the
		// project guidelines body is unchanged, but the workspace
		// instructions must never come from the previous project.
		a.RefreshWorkspaceInstructions(dir)
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
	if len(a.Messages) <= a.Context.CompactKeepRecentMessages()+1 {
		return fmt.Errorf("not enough history to compact (%d messages)", len(a.Messages))
	}
	a.statsMu.RLock()
	cachedCounts := append([]int(nil), a.tokenCounts...)
	a.statsMu.RUnlock()
	compacted, newPins, err := a.Context.CompactPinned(ctx, a.systemPromptPrefix(), a.Messages, cachedCounts, pinnedSet(a.PinManager))
	if err != nil {
		return err
	}
	// Publish the compacted history together with its freshly computed token
	// counts (cheap — compaction shrank the conversation) so the cached
	// shouldCompactUsingCounts path stays valid on the next turn.
	counts := make([]int, len(compacted))
	for i, m := range compacted {
		counts[i] = contextmgr.ComputeMessageTokens(m)
	}
	a.replaceMessagesWithCounts(compacted, counts)
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
	if err := a.requireModelSelected(ctx); err != nil {
		// The turn cannot start: the just-appended user message is dropped
		// instead of being sent to a provider with no usable model. Roll it
		// back in memory WITHOUT writing anything: the session must stay
		// exactly as it was. A fresh session must not get a file at all
		// (the old pre-check flush created one, and this rollback flush then
		// overwrote it with an empty snapshot — the "saved as an empty
		// session" bug, where a 0-message session appeared in the list and
		// became the restore target on the next startup), and a session
		// with prior content already has that content on disk.
		a.truncateMessages(1)
		// Re-derive the label from what remains only when it was derived
		// from the conversation: a session whose only message failed must
		// not keep a stale title. A deliberate rename (RenameSession /
		// session_rename) is authoritative — re-deriving it would clear the
		// rename marker and the next save would silently replace the user's
		// custom title with the first-message text.
		a.statsMu.RLock()
		renamed := a.labelRenamed
		a.statsMu.RUnlock()
		if !renamed {
			a.setSessionLabel(llm.SessionLabel(a.Messages))
		}
		a.resetSaveTracking()
		return "", err
	}
	// Persist the user message before the turn runs so a failed/cancelled
	// turn does not drop it. Deliberately AFTER the model check: a turn
	// that cannot start must not write anything (see the rollback above) —
	// the message is dropped there, so persisting it first would leave an
	// empty session file behind once it was rolled back.
	a.FlushSession()

	if h == nil {
		h = &llm.StreamHandlers{}
	}
	// Per-turn patch retry budget: a model that fails patch_file three times
	// in a row within this turn is looping; the turn is stopped instead of
	// letting it retry indefinitely.
	a.patchTurnStrikes.Store(0)
	// Per-turn context-window refusal recovery budget (Phase 3): at most
	// one forced-compaction + retry per turn.
	a.overflowRetries.Store(0)
	// Ghost-round recovery budget: at most one automatic retry of a round
	// the model ended without usable output. The counter tracks
	// CONSECUTIVE ghosts and resets after any successful round (see the
	// tool-call branch); the turn-start reset covers the first round.
	a.ghostRetries.Store(0)
	for first := true; ; first = false {
		result, err := a.runTurn(ctx, h, first)
		if err != nil {
			// Phase 3: a provider context-window refusal recovers in-loop
			// (forced compaction + one retry) instead of aborting the turn.
			// Non-overflow errors, a cancelled context, and an exhausted
			// recovery budget all fall through to the original error path.
			if retry, terminal := a.handleOverflowError(ctx, h, err); retry {
				continue
			} else if terminal != nil {
				return "", terminal
			}
			return "", err
		}

		if len(result.ToolCalls) == 0 {
			finishStreamUI(h)
			// Deliberately no OnRecoverPartialStream here, unlike the tool
			// branch below: the callback exists to reset UI state after a
			// stream error mid-tool-call, and no consumer is registered for
			// content recovery (the TUI wires a no-op, the web server wires
			// nothing). Round-end events stay single-fired — firing it here
			// too would double-deliver on every recovered content turn.
			// A result with no content, no refusal, and no tool calls is a
			// truncated turn (e.g. finish_reason="length" after consuming the
			// output budget on reasoning, or a "stop" right after
			// reasoning-only chunks). Persisting it would leave a ghost
			// assistant message that renders as an empty reply, pollutes later
			// turns, and becomes a fork point. Providers emit this
			// transiently, so the round is retried once in-loop before the
			// error surfaces to the user. The budget counts CONSECUTIVE
			// ghost rounds: any successful round (notably a tool-call
			// round, which continues the loop) resets it, so a long turn
			// gets a fresh retry after each recovery — only back-to-back
			// ghosts exhaust the cap. Nothing was appended for the ghost
			// round, so the retried request re-sends the identical view.
			// The provider-reported finish reason is included when
			// known so exhausted retries are diagnosable without
			// provider-side logs ("length" = output budget exhausted,
			// "stop" = stream ended after reasoning-only chunks).
			if result.Content == "" && result.Refusal == "" {
				if a.ghostRetries.Add(1) <= 1 {
					continue
				}
				if result.FinishReason == "" {
					return "", fmt.Errorf("model returned no output (response was truncated mid-reasoning); please try again")
				}
				return "", fmt.Errorf("model returned no output (finish_reason=%q; response was truncated mid-reasoning); please try again", result.FinishReason)
			}
			a.appendMessage(llm.Message{
				Role:      "assistant",
				Content:   result.Content,
				Reasoning: result.Reasoning,
				Refusal:   result.Refusal,
				CreatedAt: time.Now().Truncate(time.Millisecond),
				Model:     result.Model,
			})
			a.FlushSession()
			if result.Content != "" {
				return result.Content, nil
			}
			// Refusal is user-visible when the model declined without content.
			return result.Refusal, nil
		}

		// A tool-call round is a successful round: the model produced
		// usable output, so the consecutive-ghost counter restarts. Without
		// this, a long multi-round turn would spend its single retry on an
		// early transient ghost and hard-fail on a later one.
		a.ghostRetries.Store(0)

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
			Model:     result.Model,
		})

		switch outcome, stopMsg := a.runToolRound(ctx, h, result.ToolCalls); outcome {
		case toolRoundStopped:
			// A patch_file failure ended the turn (marker-only diff or the
			// per-turn mismatch budget was exhausted): return the final tool
			// result as the turn outcome so the host shows why the turn
			// ended without calling the model again.
			return stopMsg, nil
		case toolRoundCancelled:
			return "", ctx.Err()
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
		res  string
		err  error
		done bool // true when the tool returned a result (before batch cancellation)
	}
	results := make([]execResult, len(toolCalls))
	// Per-tool-call image sinks, mirroring the sequential path: read_image
	// handlers collect images into their call's sink, and each sink is
	// drained right after its tool result below, in model order, so the
	// transcript reads tool(result) → user(image) for every call.
	sinks := make([]*imageSink, len(toolCalls))
	for i := range sinks {
		sinks[i] = &imageSink{}
	}

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
			// Attach the live-output sink (if any) to this call's context,
			// exactly like the sequential path in runToolRound, so read-only
			// commands and searches running in parallel stream intermediate
			// chunks to the UI tagged with this call's identity (id/name).
			// The per-call image sink is layered on top as before.
			tc := toolCalls[i]
			toolCtx := ctx
			if h.OnToolOutput != nil {
				toolCtx = ContextWithToolOutput(toolCtx, func(command, chunk string) {
					h.OnToolOutput(tc.ID, tc.Name, command, chunk)
				})
			}
			if h.OnToolOutputEnd != nil {
				toolCtx = ContextWithToolOutputEnd(toolCtx, func(success bool) {
					h.OnToolOutputEnd(tc.ID, success)
				})
			}
			toolCtx = ContextWithImageSink(toolCtx, sinks[i])
			res, err := a.executeTool(toolCtx, tc)
			results[i] = execResult{res: res, err: err, done: true}
		}(i)
	}
	wg.Wait()

	cancelled := ctx.Err() != nil
	for i, tc := range toolCalls {
		res, errTool := results[i].res, results[i].err
		// Mirror runToolRound's per-call semantics: a tool that completed
		// before the cancellation keeps its real result; only a call that
		// was interrupted (returned context.Canceled) or never ran (still
		// waiting to start when the batch finished) reads as cancelled.
		if errTool == context.Canceled || (cancelled && !results[i].done) {
			res = "The operation was cancelled by the user."
			if h.OnToolResult != nil {
				h.OnToolResult(tc.ID, tc.Name, res, false)
			}
			a.appendToolResult(tc, res)
			a.appendImageMessages(sinks[i])
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
		a.appendImageMessages(sinks[i])
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

func stringArg(args map[string]any, key string) (string, error) {
	if _, ok := args[key]; !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	return stringArgOptional(args, key)
}

func stringArgOptional(args map[string]any, key string) (string, error) {
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
	if a.Executor != nil && !a.Executor.DeleteApprovalRequired() {
		ctx = ContextWithDeleteApprovalRequired(ctx, false)
	}
	return ctx
}

// boolArg reads an optional boolean tool argument, returning def when the
// key is absent.
func boolArg(args map[string]any, key string, def bool) (bool, error) {
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

func intArgOptional(args map[string]any, key string) (int, error) {
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
	case string:
		// Models sometimes quote numeric arguments (e.g. "id": "3").
		// Coerce a string that parses as a plain integer; anything else
		// (fractions, exponents, non-numeric text) still errors rather
		// than silently becoming 0.
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("argument %q must be an integer", key)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("argument %q must be an integer", key)
	}
}

// intRequiredArg reads a required positive-integer tool argument, e.g. an
// item id. Unlike intArgOptional it errors when the key is absent or the
// value is not a positive integer (ids start at 1, so 0 is invalid). It
// accepts the same value shapes intArgOptional does: JSON numbers
// (float64), int/int64, and quoted numeric strings.
func intRequiredArg(args map[string]any, key string) (int, error) {
	if _, ok := args[key]; !ok {
		return 0, fmt.Errorf("missing required argument %q", key)
	}
	n, err := intArgOptional(args, key)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("argument %q must be a positive integer", key)
	}
	return n, nil
}

func stringSliceArg(args map[string]any, key string) ([]string, error) {
	if _, ok := args[key]; !ok {
		return nil, fmt.Errorf("missing required argument %q", key)
	}
	return stringSliceArgOptional(args, key)
}

func stringSliceArgOptional(args map[string]any, key string) ([]string, error) {
	val, ok := args[key]
	if !ok {
		return nil, nil
	}
	return coerceStringSlice(key, val)
}

func coerceStringSlice(key string, val any) ([]string, error) {
	switch v := val.(type) {
	case []string:
		return v, nil
	case []any:
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
