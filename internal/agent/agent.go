package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"strings"
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
	UsageAccum     UsageAccumulator
	lastTurnUsage  *llm.Usage
	lastPersistErr error
	// sessionDirty tracks whether in-memory state differs from disk.
	// TUI: single owner goroutine. Web server: Server.agentMu + turnMu serialize
	// access across WebSocket clients (see internal/server).
	sessionDirty    bool
	lastPersistTime time.Time // timestamp of last actual disk write

	// DebugCompareMessages enables view-fingerprint comparison across turns
	// and session restores (GOGEN_DEBUG_COMPARE_MESSAGES). Only effective in
	// binaries built with `-tags debug`; production builds compile the
	// detector out (see view_drift_release.go).
	DebugCompareMessages bool
	lastViewMessages     []llm.Message // debug builds only; unused in release

	// Runtime / project
	WorkingDir        string
	Mode              Mode
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
		Provider:     provider,
		Executor:     executor,
		Context:      ctxMgr,
		Messages:     []llm.Message{},
		WorkingDir:   executor.GetWorkingDir(),
		Mode:         ModeAct,
		toolHandlers: BuiltinToolHandlers(),
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
	cfgPath = projectfile.DefaultSavePath(a.WorkingDir)
	guidelinesPath = projectfile.DefaultGuidelinesSavePath(a.WorkingDir)
	err = projectfile.SaveConfig(cfgPath, guidelinesPath, &effective, a.ProjectGuidelines, projectfile.WriteOptions{IncludeSecrets: includeSecrets})
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
	a.doPersist()
}

// FlushSession forces an immediate disk write regardless of debounce timing.
// Use at final boundaries: turn complete, errors, context cancellation, and quit.
func (a *Agent) FlushSession() {
	a.sessionDirty = true
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	a.doPersist()
}

// doPersist is the actual write — called by persistSession/flushSession.
// Callers (persistSession, FlushSession) already validate SessionStore/SessionID;
// this method only checks the dirty flag.
func (a *Agent) doPersist() {
	// Skip if nothing changed since the last save.
	if !a.sessionDirty {
		return
	}
	profile := a.ensureProjectProfile()
	// Only copy messages now that we know we'll write.
	msgs := append([]llm.Message(nil), a.Messages...)
	if err := a.SessionStore.Save(a.SessionID, SessionSnapshot{
		WorkingDir:     a.WorkingDir,
		Model:          a.CurrentModel(),
		Mode:           a.Mode.String(),
		Label:          a.SessionLabel,
		ProjectProfile: profile,
		Todos:          todoSnapshot(a.TodoManager),
		Messages:       msgs,
	}); err != nil {
		log.Printf("session save failed (id=%s): %v", a.SessionID, err)
		a.lastPersistErr = err
		return
	}
	a.lastPersistErr = nil
	a.lastPersistTime = time.Now()
	a.sessionDirty = false
}

// ConsumePersistError returns and clears the last session save failure, if any.
func (a *Agent) ConsumePersistError() error {
	err := a.lastPersistErr
	a.lastPersistErr = nil
	return err
}

// SetWorkingDir updates the agent and executor working directory together.
func (a *Agent) SetWorkingDir(dir string) {
	a.WorkingDir = dir
	a.projectProfile = ""
	if a.Executor != nil {
		a.Executor.SetWorkingDir(dir)
	}
	if a.TodoManager != nil {
		a.TodoManager.SetWorkingDir(dir)
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

func newToolCallID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
					if a.PinManager != nil {
						a.PinManager.ReplacePins(newPins)
					}
					// lastTurnUsage is no longer representative after compaction.
					a.lastTurnUsage = nil
				}
			}
		}
		a.Context.EnsureToolResultsCapped(a.Messages)
		view = a.Messages
	}
	view = withSystemPrompt(view, a.WorkingDir)
	view = enrichSystemPrompt(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)

	// Pin wire-stable tool args before compare/snapshot/send so the view,
	// history, and HTTP body share one ArgsStr (and the detector does not
	// false-positive when messagesToChat would have written ArgsStr later).
	stabilizeViewToolArgs(view)

	a.recordViewForDrift(view)
	return view
}

func stabilizeViewToolArgs(view []llm.Message) {
	for i := range view {
		for j := range view[i].ToolCalls {
			llm.StabilizeToolCallArgs(&view[i].ToolCalls[j])
		}
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
	// lastTurnUsage is no longer representative after compaction.
	a.lastTurnUsage = nil
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
	})
}

// StreamProcessInput streams tokens to the handlers as they arrive.
// It returns the final accumulated response or an error.
func (a *Agent) StreamProcessInput(ctx context.Context, input string, h *llm.StreamHandlers) (string, error) {
	a.Messages = append(a.Messages, llm.Message{Role: "user", Content: input})
	// Persist immediately so a failed/cancelled turn does not drop the user message.
	a.FlushSession()

	if err := a.requireModelSelected(ctx); err != nil {
		a.Messages = a.Messages[:len(a.Messages)-1]
		a.FlushSession()
		return "", err
	}

	if h == nil {
		h = &llm.StreamHandlers{}
	}

	for first := true; ; first = false {
		if ctx.Err() != nil {
			finishStreamUI(h)
			a.FlushSession()
			return "", ctx.Err()
		}
		view := a.prepareMessages(ctx)

		if first && h.OnStart != nil {
			h.OnStart()
		} else if !first && h.OnRoundStart != nil {
			h.OnRoundStart()
		}

		result, err := a.Provider.GenerateResponseStream(ctx, view, a.AllowedToolNames(), a.llmTools(), h)
		if err != nil {
			finishStreamUI(h)
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

		a.Messages = append(a.Messages, llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			Reasoning: result.Reasoning,
			Refusal:   result.Refusal,
			ToolCalls: result.ToolCalls,
		})

		for i, tc := range result.ToolCalls {
			if ctx.Err() != nil {
				// Preserve a valid tool-call/result protocol for the next turn.
				a.appendCanceledToolResults(result.ToolCalls[i:], ctx.Err())
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
					errTool = ctx.Err()
				}
				res = formatToolError(res, errTool)
				if h.OnToolResult != nil {
					h.OnToolResult(tc.ID, tc.Name, res, false)
				}
				a.appendToolResult(tc, res)
				a.appendCanceledToolResults(result.ToolCalls[i+1:], ctx.Err())
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

func (a *Agent) appendCanceledToolResults(toolCalls []llm.ToolCall, err error) {
	msg := formatToolError("", err)
	for _, tc := range toolCalls {
		a.appendToolResult(tc, msg)
	}
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
