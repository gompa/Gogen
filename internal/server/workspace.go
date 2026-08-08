package server

import (
	"context"
	"sync"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/modelinfo"
	"gogen/internal/session"
)

// Workspace is the shared, session-independent state of the web server
// (multi-session plan §2). Every session in one web process shares a single
// Workspace: the executor (working dir + secure path + command guard), the
// session store, the config, the MCP registry, and a provider factory. The
// factory creates a fresh provider per session (each owns its own
// model/thinking-level state, E6) seeded from the workspace default model
// and thinking level (D1 — the model is per-session).
type Workspace struct {
	Exec   *agent.Executor
	Store  *session.Store
	Config *config.Config

	GlobalMode bool
	// Project context shared by all session agents.
	ProjectFilePath   string
	ProjectGuidelines string
	TestCommand       string
	LintCommand       string

	DebugCompareMessages bool
	// MCPRegistry is shared by all session agents (thread-safe, E20).
	MCPRegistry agent.MCPToolRegistry

	// Model is the workspace default model: new providers are seeded
	// with it. set_model never mutates it — each pane's model lives on its
	// own provider instance.
	Model string
	// ThinkingLevel is the workspace-level default thinking level new
	// sessions start with (per-session afterwards).
	ThinkingLevel string

	// ProviderFactory creates a per-session provider. It must seed the
	// provider with the workspace Model + ThinkingLevel so a new pane's
	// first turn never fails requireModelSelected.
	ProviderFactory func() llm.LLMProvider

	// Resolver is the shared models.dev resolver. One per workspace so N
	// per-session providers do not issue N parallel catalog fetches or race
	// N atomic writes to the same .gogen/models.json cache file.
	Resolver *modelinfo.Resolver

	// wdMu guards WorkingDir. The field is written by handleWSConfig on one
	// connection's WS read loop (working-dir change) and read concurrently
	// from other connections' read loops (session ops, NewSessionAgent) and
	// from per-session provider construction — a plain field access raced
	// those readers (data race under -race). Production access must go
	// through GetWorkingDir/SetWorkingDir; the field stays exported only so
	// existing tests can read it directly.
	wdMu       sync.RWMutex
	WorkingDir string

	// fsMu serializes filesystem-mutating operations across sessions:
	// editor writes (fs_write/fs_replace/fs_apply_patch) and agent tools
	// that mutate the tree. Lock order: session turnMu → fsMu; never the
	// reverse (editor writes take fsMu alone).
	fsMu sync.RWMutex
}

// GetWorkingDir returns the workspace-wide working directory.
// Thread-safe: read by session lifecycle ops and provider construction on
// any connection's read loop while handleWSConfig may update it.
func (ws *Workspace) GetWorkingDir() string {
	if ws == nil {
		return ""
	}
	ws.wdMu.RLock()
	defer ws.wdMu.RUnlock()
	return ws.WorkingDir
}

// SetWorkingDir updates the workspace-wide working directory.
// Thread-safe. Does NOT touch the shared executor or any session agent —
// handleWSConfig applies the change to every session agent (under each
// session's turn lock) via applyWorkingDirToAll.
func (ws *Workspace) SetWorkingDir(dir string) {
	if ws == nil {
		return
	}
	ws.wdMu.Lock()
	ws.WorkingDir = dir
	ws.wdMu.Unlock()
}

// fsMutatingTools are agent tools that mutate the working tree. They take the
// workspace fsMu during execution (lock order: turnMu → fsMu). Read-only
// tools and non-FS tools (execute_command, git mutations, web fetch/search)
// are deliberately NOT classified here: git mutations are serialized by the
// executor/git itself, and execute_command must not block editor saves for
// its whole runtime (multi-session plan §2.4).
//
// The set is derived from the MutatesFS flag in the agent tool registry
// (agent.FSMutatingToolNames), so a mutating tool is locked by declaration in
// its ToolDef — no second hand-maintained map to drift.
var fsMutatingTools = func() map[string]bool {
	names := agent.FSMutatingToolNames()
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}()

// wrapToolHandlers returns a copy of handlers in which every FS-mutating tool
// acquires fsMu for the duration of its execution. Non-mutating handlers are
// passed through untouched.
func wrapToolHandlers(handlers map[string]agent.ToolHandler, fsMu *sync.RWMutex) map[string]agent.ToolHandler {
	out := make(map[string]agent.ToolHandler, len(handlers))
	for name, h := range handlers {
		if !fsMutatingTools[name] {
			out[name] = h
			continue
		}
		name, h := name, h
		out[name] = func(ctx context.Context, a *agent.Agent, args map[string]interface{}) (string, error) {
			fsMu.Lock()
			defer fsMu.Unlock()
			return h(ctx, a, args)
		}
	}
	return out
}

// NewSessionAgent creates a fresh session agent from the workspace: a new
// per-session provider + context manager, sharing the workspace executor,
// store, MCP registry, and fsMu-wrapped tool handlers. When snap is non-nil
// the agent is restored from it under the given id (multi-session plan §2,
// session agent factory).
func (ws *Workspace) NewSessionAgent(snap *agent.SessionSnapshot, id string) *agent.Agent {
	settings := contextmgr.DefaultSettings()
	if ws.Config != nil {
		settings = contextmgr.Settings{
			ContextLimit:              ws.Config.ContextLimit,
			CompactThreshold:          ws.Config.CompactThreshold,
			CompactKeepRecentMessages: ws.Config.CompactKeepRecentMessages,
			MaxToolResultBytes:        ws.Config.MaxToolResultBytes,
			CompactReserveTokens:      ws.Config.CompactReserveTokens,
		}
	}
	provider := ws.ProviderFactory()
	ctxMgr := contextmgr.NewManager(provider, settings)
	a := agent.NewAgent(provider, ws.Exec, ctxMgr)
	a.GlobalMode = ws.GlobalMode
	a.SetProjectContext(ws.ProjectFilePath, ws.ProjectGuidelines, ws.TestCommand, ws.LintCommand)
	// The workspace dir is authoritative for the agent's own WorkingDir and
	// the shared executor: NewAgent seeds both from the executor, which may
	// lag ws.WorkingDir in the window between a client's working-dir change
	// (handleWSConfig updates ws.WorkingDir first) and the per-session sweep
	// (applyWorkingDirToAll). Seeding here closes the gap so a session
	// created in that window is consistent from birth — the sweep's
	// SetWorkingDir for it later is then a no-op.
	workingDir := ws.GetWorkingDir()
	a.SetWorkingDir(workingDir)
	a.TodoManager = agent.NewTodoManager(workingDir)
	a.PinManager = agent.NewPinManager()
	a.DebugCompareMessages = ws.DebugCompareMessages
	a.SessionStore = ws.Store
	a.SessionID = id
	a.SetMCPRegistry(ws.MCPRegistry)
	a.SetToolHandlers(wrapToolHandlers(agent.BuiltinToolHandlers(), &ws.fsMu))
	if snap != nil {
		a.RestoreSession(*snap, id)
		// D1: model is per-session — a resumed session keeps its saved model
		// (RestoreSessionLocal already calls Provider.SetModel(snap.Model));
		// never overwrite it with the workspace default. Validate the saved
		// model asynchronously: refresh the context limit if the snapshot had
		// none pre-warmed, and drop the model if the provider no longer lists
		// it (so requireModelSelected surfaces the gap on the first turn).
		if snap.Model != "" {
			go a.ValidateRestoredModel(context.Background(), snap.Model)
		}
		// A restored session keeps its saved thinking level
		// (RestoreSessionLocal restores it and syncs the provider). Seed the
		// workspace default only when the snapshot predates the level field
		// — and NEVER before the restore: SetThinkingLevel flushes, and a
		// flush before the restore writes an EMPTY snapshot over the
		// session's real file, wiping its persisted messages and index label
		// (the "closing an open session turns its title into a hash" bug —
		// merely opening a saved session corrupted its on-disk state until
		// the next flush with restored content).
		if snap.ThinkingLevel == "" {
			if level, ok := agent.ParseThinkingLevel(ws.ThinkingLevel); ok {
				a.SetThinkingLevel(level)
			}
		}
	} else {
		// Fresh session: seed the workspace default thinking level so a
		// new pane's first turn does not start at the "off" default.
		if level, ok := agent.ParseThinkingLevel(ws.ThinkingLevel); ok {
			a.SetThinkingLevel(level)
		}
	}
	return a
}

// newWorkspaceFromAgent builds a Workspace that shares the given agent's
// executor and store. Used by NewServer while the server hosts the default
// session (the agent itself is registered directly; NewSessionAgent is only
// exercised once later phases create additional sessions). The provider
// factory seeds each new provider from the agent's current model/thinking.
func newWorkspaceFromAgent(a *agent.Agent, cfg *config.Config) *Workspace {
	ws := &Workspace{
		Exec:                 a.Executor,
		Config:               cfg,
		GlobalMode:           a.GlobalMode,
		ProjectFilePath:      a.ProjectFilePath,
		ProjectGuidelines:    a.ProjectGuidelines,
		TestCommand:          a.TestCommand,
		LintCommand:          a.LintCommand,
		DebugCompareMessages: a.DebugCompareMessages,
		MCPRegistry:          a.MCPRegistry,
		Model:                a.CurrentModel(),
		ThinkingLevel:        string(a.ThinkingLevel),
		WorkingDir:           a.Executor.GetWorkingDir(),
	}
	if st, ok := a.SessionStore.(*session.Store); ok {
		ws.Store = st
	}
	ws.Resolver = modelinfo.NewResolver(modelinfo.CachePath(ws.WorkingDir))
	if _, ok := a.Provider.(*llm.OpenAIProvider); ok {
		// Production path (main.go): every session gets a fresh provider
		// seeded with the workspace default model + thinking level, sharing
		// the workspace models.dev resolver (E6, D1).
		ws.ProviderFactory = func() llm.LLMProvider {
			wd := ws.GetWorkingDir()
			p := llm.NewOpenAIProviderWithResolver(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL, wd, ws.Resolver)
			p.SetPromptCacheKey(llm.ProjectPromptCacheKey(wd))
			p.SetPreserveReasoningMode(cfg.PreserveReasoning)
			if ws.Model != "" {
				_ = p.SetModel(ws.Model)
			}
			p.SetThinkingLevel(ws.ThinkingLevel)
			return p
		}
	} else {
		// Test/embed path: the agent was built with a custom provider (mock
		// or stub) that has no mutable model/thinking state to isolate, so
		// new sessions share it. E6's per-session isolation only matters for
		// the real OpenAIProvider.
		ws.ProviderFactory = func() llm.LLMProvider {
			return a.Provider
		}
	}
	return ws
}
