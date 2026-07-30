package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
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
	lastTurnUsage  *llm.Usage
	// apiBaselinePromptTokens and apiBaselineMsgCount let ContextStats use the
	// API's exact prompt_tokens as the authoritative baseline for Snapshot.Used,
	// only estimating messages added after the last API round.
	apiBaselinePromptTokens, apiBaselineMsgCount int
	lastPersistErr                               error
	// sessionDirty tracks whether in-memory state differs from disk.
	// TUI: single owner goroutine. Web server: Server.agentMu + turnMu serialize
	// access across WebSocket clients (see internal/server).
	sessionDirty    bool
	lastPersistTime time.Time // timestamp of last actual disk write
	// lastSavedMsgCount tracks how many messages were included in the last
	// full snapshot save. Used by doPersist to decide between full and
	// incremental delta saves, avoiding full JSON serialization every 5s.
	lastSavedMsgCount int
	lastFullSaveTime  time.Time // when the last full snapshot was written

	// DebugCompareMessages enables view-fingerprint comparison across turns
	// and session restores (GOGEN_DEBUG_COMPARE_MESSAGES). Only effective in
	// binaries built with `-tags debug`; production builds compile the
	// detector out (see view_drift_release.go).
	DebugCompareMessages bool
	lastViewMessages     []llm.Message // debug builds only; unused in release

	// restoredTokenCounts holds pre-computed token counts from a restored
	// session snapshot. When non-nil and len matches a.Messages, ContextStats
	// uses these counts instead of re-tokenizing every message, which is
	// expensive for large sessions. Cleared when messages are modified.
	restoredTokenCounts []int

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
	toolHandlers      map[string]ToolHandler
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
	a.projectProfile = ""
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
	a.sessionDirty = true
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	// Skip if debounced — no point computing hash or doing I/O.
	if !a.lastPersistTime.IsZero() && time.Since(a.lastPersistTime) < persistMinInterval {
		return
	}
	a.doPersist(false)
}

// FlushSession forces an immediate disk write regardless of debounce timing.
// Use at final boundaries: turn complete, errors, context cancellation, and quit.
// Skips full re-tokenization so Ctrl+C / --web shutdown stays snappy on large
// sessions; restored counts are reused when still valid.
func (a *Agent) FlushSession() {
	a.sessionDirty = true
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
	if !a.sessionDirty {
		return
	}
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	// Extend cached token counts to cover any new messages so the full
	// snapshot path can reuse them instead of re-tokenizing everything.
	a.extendTokenCounts()

	count := len(a.Messages)
	profile := a.ensureProjectProfile()

	// Safety: if the message list was truncated since last save (e.g.
	// compaction, error rollback), force a full snapshot.
	if a.lastSavedMsgCount > count {
		a.lastSavedMsgCount = 0
	}

	// Decide: full snapshot or incremental delta?
	// Full snapshot on first save, when more than 5 new messages have
	// arrived, or when it's been >30s since the last full snapshot.
	needsFullSave := a.lastSavedMsgCount == 0 ||
		count-a.lastSavedMsgCount > 5 ||
		time.Since(a.lastFullSaveTime) > 30*time.Second

	if needsFullSave {
		msgs := append([]llm.Message(nil), a.Messages...)
		snap := SessionSnapshot{
			WorkingDir:     a.WorkingDir,
			Model:          a.CurrentModel(),
			Mode:           a.Mode.String(),
			ThinkingLevel:  string(a.ThinkingLevel),
			Oneshot:        a.SessionOneshot,
			Label:          a.SessionLabel,
			ProjectProfile: profile,
			Todos:          todoSnapshot(a.TodoManager),
			Messages:       msgs,
			ContextLimit:   a.ContextLimit(),
		}
		if len(a.restoredTokenCounts) == len(msgs) {
			snap.TokenCounts = append([]int(nil), a.restoredTokenCounts...)
		} else if a.Context != nil && !skipTokenCounts {
			snap.TokenCounts = a.Context.TokenCounts(msgs)
		}
		if err := a.SessionStore.Save(a.SessionID, snap); err != nil {
			log.Printf("session save failed (id=%s): %v", a.SessionID, err)
			a.lastPersistErr = err
			return
		}
		a.lastSavedMsgCount = count
		a.lastFullSaveTime = time.Now()
	} else {
		// Incremental: only serialise new messages since the last full save.
		newMsgs := a.Messages[a.lastSavedMsgCount:]
		var newCounts []int
		if a.Context != nil && !skipTokenCounts {
			newCounts = make([]int, len(newMsgs))
			for i := range newMsgs {
				newCounts[i] = contextmgr.ComputeMessageTokens(newMsgs[i])
			}
		}
		deltaSnap := SessionSnapshot{
			WorkingDir:  a.WorkingDir,
			Oneshot:     a.SessionOneshot,
			Label:       a.SessionLabel,
			Messages:    newMsgs,
			TokenCounts: newCounts,
		}
		if err := a.SessionStore.AppendMessages(a.SessionID, deltaSnap); err != nil {
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
	a.sessionDirty = false
}

// resetSaveTracking resets the incremental-save counters so the next
// doPersist writes a full snapshot. Call after any operation that
// truncates or replaces a.Messages (compaction, session restore, etc.).
func (a *Agent) resetSaveTracking() {
	a.lastSavedMsgCount = 0
	a.lastFullSaveTime = time.Time{}
}

// extendTokenCounts extends restoredTokenCounts to cover any messages
// appended since the last restore or extension. Call after appending to
// a.Messages when restoredTokenCounts is non-nil. This preserves the
// fast SnapshotWithCounts path in ContextStats and avoids re-tokenizing
// the entire history on every call.
func (a *Agent) extendTokenCounts() {
	if a.restoredTokenCounts == nil {
		return
	}
	for len(a.restoredTokenCounts) < len(a.Messages) {
		idx := len(a.restoredTokenCounts)
		a.restoredTokenCounts = append(a.restoredTokenCounts,
			contextmgr.ComputeMessageTokens(a.Messages[idx]))
	}
}

// ConsumePersistError returns and clears the last session save failure, if any.
func (a *Agent) ConsumePersistError() error {
	err := a.lastPersistErr
	a.lastPersistErr = nil
	return err
}

// SetWorkingDir updates in-memory working directory fields (agent, executor,
// todos). It does not touch disk or the models.dev cache — call
// AfterWorkingDirChange for that, and never while holding server agentMu.
func (a *Agent) SetWorkingDir(dir string) {
	a.WorkingDir = dir
	a.projectProfile = ""
	if a.Executor != nil {
		a.Executor.SetWorkingDir(dir)
	}
	if a.TodoManager != nil {
		a.TodoManager.SetWorkingDir(dir)
	}
}

// AfterWorkingDirChange persists the session and retargets the models.dev
// cache for the new project dir. Must not run under server agentMu — both
// steps do disk (and possibly background network) I/O.
func (a *Agent) AfterWorkingDirChange() {
	cacheDir := a.WorkingDir
	if a.GlobalMode {
		cacheDir = projectfile.GlobalDataDir()
	}
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok {
		p.SetModelInfoCacheDir(cacheDir)
	}
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

func (a *Agent) ensureProjectProfile() string {
	if a.projectProfile != "" {
		return a.projectProfile
	}
	a.projectProfile = DetectProjectProfile(a.WorkingDir, a.TestCommand, a.LintCommand)
	return a.projectProfile
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

func (a *Agent) prepareMessages(ctx context.Context) []llm.Message {
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
			if a.Context.ShouldCompact(a.Messages) {
				var pinned map[int]struct{}
				if a.PinManager != nil {
					pinned = a.PinManager.PinnedSet()
				}
				compacted, newPins, err := a.Context.CompactPinned(ctx, a.Messages, pinned)
				if err == nil {
					a.Messages = compacted
					// Messages replaced — cached token counts invalid.
					a.restoredTokenCounts = nil
					if a.PinManager != nil {
						a.PinManager.ReplacePins(newPins)
					}
					// lastTurnUsage is no longer representative after compaction.
					a.clearTurnUsage()
					a.resetSaveTracking()
				}
			}
		}
		a.Context.EnsureToolResultsCapped(a.Messages)
		view = a.Messages
	}
	// Stabilize tool args on a.Messages (not view, which may be a copy) so
	// ArgsStabilized is persisted and we skip already-stable messages.
	stabilizeToolArgs(a.Messages)

	view = withSystemPrompt(view, a.WorkingDir)
	view = enrichSystemPrompt(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)

	a.recordViewForDrift(view)
	return view
}

// stabilizeToolArgs ensures every unstabilized tool call has its ArgsStr set.
// Skipped messages with ArgsStabilized=true — this turns an O(total_tool_calls)
// scan into O(new_tool_calls) per turn.
func stabilizeToolArgs(msgs []llm.Message) {
	for i := range msgs {
		if msgs[i].ArgsStabilized {
			continue
		}
		for j := range msgs[i].ToolCalls {
			llm.StabilizeToolCallArgs(&msgs[i].ToolCalls[j])
		}
		msgs[i].ArgsStabilized = true
	}
}

// CompactHistory manually compacts conversation history at a task boundary.
func (a *Agent) CompactHistory(ctx context.Context) error {
	if a.Context == nil {
		return fmt.Errorf("context management is not configured")
	}
	if len(a.Messages) <= a.Context.Settings.KeepRecentMessages+1 {
		return fmt.Errorf("not enough history to compact (%d messages)", len(a.Messages))
	}
	compacted, newPins, err := a.Context.CompactPinned(ctx, a.Messages, pinnedSet(a.PinManager))
	if err != nil {
		return err
	}
	a.Messages = compacted
	if a.PinManager != nil {
		a.PinManager.ReplacePins(newPins)
	}
	// Compacted messages have different content — cached token counts invalid.
	a.restoredTokenCounts = nil
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
	a.Messages = append(a.Messages, llm.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: tc.ID,
		CreatedAt:  time.Now().Truncate(time.Millisecond),
	})
	a.extendTokenCounts()
}

// StreamProcessInput streams tokens to the handlers as they arrive.
// It returns the final accumulated response or an error.
func (a *Agent) StreamProcessInput(ctx context.Context, input string, h *llm.StreamHandlers) (string, error) {
	a.Messages = append(a.Messages, llm.Message{Role: "user", Content: input, CreatedAt: time.Now().Truncate(time.Millisecond)})
	a.extendTokenCounts()
	// If the session doesn't have a label yet, derive one from the first user message.
	if a.SessionLabel == "" {
		a.SessionLabel = llm.SessionLabel(a.Messages, llm.DefaultSessionLabelMaxLen)
	}
	// Persist immediately so a failed/cancelled turn does not drop the user message.
	a.FlushSession()

	if err := a.requireModelSelected(ctx); err != nil {
		a.Messages = a.Messages[:len(a.Messages)-1]
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
		view := a.prepareMessages(ctx)

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
		a.UsageAccum.Add(result.Usage)

		if len(result.ToolCalls) == 0 {
			finishStreamUI(h)
			a.Messages = append(a.Messages, llm.Message{
				Role:      "assistant",
				Content:   result.Content,
				Reasoning: result.Reasoning,
				Refusal:   result.Refusal,
				CreatedAt: time.Now().Truncate(time.Millisecond),
			})
			a.extendTokenCounts()
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

		a.Messages = append(a.Messages, llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			Reasoning: result.Reasoning,
			Refusal:   result.Refusal,
			ToolCalls: result.ToolCalls,
			CreatedAt: time.Now().Truncate(time.Millisecond),
		})
		a.extendTokenCounts()

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

			res, errTool := a.executeTool(ctx, tc)
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
		a.persistSession()
	}
}

func (a *Agent) appendCanceledToolResults(toolCalls []llm.ToolCall) {
	const msg = "Tool execution was skipped because the user cancelled the operation."
	for _, tc := range toolCalls {
		a.appendToolResult(tc, msg)
	}
}

// RepairOrphanToolCalls appends cancelled tool-result placeholders for any
// trailing assistant tool_calls that lack matching tool messages. Call on
// cancel/shutdown so the next turn keeps a valid tool-call/result protocol.
// Returns true when messages were modified.
func (a *Agent) RepairOrphanToolCalls() bool {
	if a == nil || len(a.Messages) == 0 {
		return false
	}
	modified := false
	for i := len(a.Messages) - 1; i >= 0; i-- {
		msg := a.Messages[i]
		if msg.Role == "tool" {
			continue
		}
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		have := make(map[string]struct{})
		for j := i + 1; j < len(a.Messages); j++ {
			if a.Messages[j].Role != "tool" {
				break
			}
			if id := a.Messages[j].ToolCallID; id != "" {
				have[id] = struct{}{}
			}
		}
		var missing []llm.ToolCall
		for _, tc := range msg.ToolCalls {
			if _, ok := have[tc.ID]; !ok {
				missing = append(missing, tc)
			}
		}
		if len(missing) == 0 {
			continue
		}
		a.appendCanceledToolResults(missing)
		modified = true
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

func boolArgOptional(args map[string]interface{}, key string) (bool, error) {
	val, ok := args[key]
	if !ok {
		return false, nil
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("argument %q must be a boolean", key)
	}
	return b, nil
}

func boolArgDefault(args map[string]interface{}, key string, def bool) (bool, error) {
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
