package agent

import (
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// SessionAgentOptions carries the shared per-session agent wiring used by
// NewSessionAgent. Both the web server (Workspace.NewSessionAgent) and the
// TUI subagent spawner build one options struct and call the same factory, so
// every session — interactive or nested — is wired identically.
type SessionAgentOptions struct {
	// Provider is the session's own provider (fresh per session in web mode;
	// shared or fresh in the TUI).
	Provider llm.LLMProvider
	// Executor is the shared workspace executor (working dir + guards).
	Executor *Executor
	// Store is the shared session store; nil disables persistence.
	Store SessionPersister
	// Config supplies context-manager settings only (nil → defaults). Live
	// feature flags are passed explicitly below, NOT read from Config: the
	// web toggle mutates workspace/agent state, never the immutable Config.
	Config *config.Config

	GlobalMode        bool
	ProjectFilePath   string
	ProjectGuidelines string
	TestCommand       string
	LintCommand       string
	WorkingDir        string
	MCPRegistry       MCPToolRegistry
	// ToolHandlers overrides the builtin handler map (e.g. the web server's
	// fsMu-wrapped copy). nil → BuiltinToolHandlers.
	ToolHandlers         map[string]ToolHandler
	DebugCompareMessages bool
	// ThinkingLevel is seeded on fresh sessions and on snapshots that
	// predate the persisted thinking-level field.
	ThinkingLevel ThinkingLevel

	// Live feature flags (board / subagent / nesting depth). The web server
	// passes its workspace values so sessions created after a live toggle
	// agree with the rest; the TUI passes its startup config values.
	BoardEnabled     bool
	SubagentsEnabled bool
	SubagentMaxDepth int
	// BoardManager is the shared project board (nil when disabled). The web
	// server passes its single workspace manager so all sessions share one;
	// the TUI creates one per process.
	BoardManager *BoardManager
	// SubagentSpawner runs nested sessions (nil when unavailable; the
	// subagent tool additionally requires the feature flag).
	SubagentSpawner SubagentSpawner
}

// NewSessionAgent creates a fresh session agent: a new context manager over
// the given provider, sharing the executor, store, MCP registry, and tool
// handlers. When snap is non-nil the agent is restored from it under the
// given id (multi-session plan §2, session agent factory).
func NewSessionAgent(opts SessionAgentOptions, snap *SessionSnapshot, id string) *Agent {
	settings := contextmgr.DefaultSettings()
	if opts.Config != nil {
		settings = contextmgr.Settings{
			ContextLimit:              opts.Config.ContextLimit,
			CompactThreshold:          opts.Config.CompactThreshold,
			CompactKeepRecentMessages: opts.Config.CompactKeepRecentMessages,
			MaxToolResultBytes:        opts.Config.MaxToolResultBytes,
			CompactReserveTokens:      opts.Config.CompactReserveTokens,
		}
	}
	ctxMgr := contextmgr.NewManager(opts.Provider, settings)
	a := NewAgent(opts.Provider, opts.Executor, ctxMgr)
	a.GlobalMode = opts.GlobalMode
	a.SetProjectContext(opts.ProjectFilePath, opts.ProjectGuidelines, opts.TestCommand, opts.LintCommand)
	// The workspace dir is authoritative for the agent's own WorkingDir and
	// the shared executor: NewAgent seeds both from the executor, which may
	// lag the workspace working dir in the window between a client's
	// working-dir change (handleWSConfig updates ws.WorkingDir first) and
	// the per-session sweep (applyWorkingDirToAll). Seeding here closes the
	// gap so a session created in that window is consistent from birth —
	// the sweep's SetWorkingDir for it later is then a no-op.
	a.SetWorkingDir(opts.WorkingDir)
	a.TodoManager = NewTodoManager(opts.WorkingDir)
	a.PinManager = NewPinManager()
	a.DebugCompareMessages = opts.DebugCompareMessages
	a.SessionStore = opts.Store
	a.SessionID = id
	a.SetMCPRegistry(opts.MCPRegistry)
	if opts.ToolHandlers != nil {
		a.SetToolHandlers(opts.ToolHandlers)
	}
	a.SetBoardEnabled(opts.BoardEnabled)
	a.SetSubagentsEnabled(opts.SubagentsEnabled)
	a.SetSubagentMaxDepth(opts.SubagentMaxDepth)
	a.SetBoardManager(opts.BoardManager)
	a.SetSubagentSpawner(opts.SubagentSpawner)
	if snap != nil {
		a.RestoreSession(*snap, id)
		// D1: model is per-session — a resumed session keeps its saved model
		// (RestoreSessionLocal already calls Provider.SetModel(snap.Model));
		// never overwrite it with the workspace default. Validate the saved
		// model asynchronously via the runtime owner after registration (see
		// loadOrCreateRuntime / sessionFork): the runtime broadcasts the
		// refresh to attached clients, and requireModelSelected surfaces the
		// gap on the first turn when the provider no longer lists the model.
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
			if level := NormalizeThinkingLevel(string(opts.ThinkingLevel)); level != "" {
				a.SetThinkingLevel(level)
			}
		}
	} else {
		// Fresh session: seed the workspace default thinking level so a
		// new pane's first turn does not start at the "off" default.
		if level := NormalizeThinkingLevel(string(opts.ThinkingLevel)); level != "" {
			a.SetThinkingLevel(level)
		}
	}
	return a
}
