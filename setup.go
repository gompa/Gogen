package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/projectfile"
	"gogen/internal/session"
)

// newAgent builds the provider, context manager, executor, agent, and
// session store from the merged config, then restores the most recent saved
// session and runs the legacy-todos migration. It returns the agent and the
// model name restored from disk (empty when session persistence is disabled
// or nothing was saved). The caller owns the agent's lifecycle defers
// (a.Close, a.FlushPending, MCP shutdown) so the existing shutdown order is
// preserved. The session store stays internal to setup: main never reads it
// after construction.
func newAgent(cfg *config.Config, isGlobalMode bool) (*agent.Agent, string) {
	// The provider carries ALL registered OpenAI-compatible profiles (the
	// legacy fields form the implicit default profile), so the TUI's /models
	// list and the web model picker both aggregate every endpoint, and each
	// model routes to its owning endpoint.
	provider := llm.NewOpenAIProviderWithProfiles(
		llm.ProviderProfiles(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL, cfg.OpenAIProviders),
		cfg.OpenAIModel, cfg.WorkingDir, nil)

	// Derive a stable prompt-cache key from the working directory so
	// provider-side prefix caches survive sequential requests.
	provider.SetPromptCacheKey(llm.ProjectPromptCacheKey(cfg.WorkingDir))
	provider.SetPreserveReasoningMode(cfg.PreserveReasoning)

	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              cfg.ContextLimit,
		CompactThreshold:          cfg.CompactThreshold,
		CompactKeepRecentMessages: cfg.CompactKeepRecentMessages,
		MaxToolResultBytes:        cfg.MaxToolResultBytes,
		CompactReserveTokens:      cfg.CompactReserveTokens,
	})

	exec := agent.NewExecutorWithGuard(cfg.WorkingDir, agent.NewCommandGuard(cfg.CommandSafetyMode, agent.ParseAllowlist(cfg.CommandAllowlist)))
	exec.SetDeleteApproval(!strings.EqualFold(cfg.DeleteApproval, "off"))
	exec.SetSandbox(cfg.CommandSandbox)
	if cfg.CommandTimeoutSecs > 0 {
		exec.SetCommandTimeout(time.Duration(cfg.CommandTimeoutSecs) * time.Second)
	}
	if isGlobalMode {
		// In global mode, relax the path boundary to the user's home directory.
		exec.PathBoundary = projectfile.HomeDir()
	}
	a := agent.NewAgent(provider, exec, ctxMgr)
	a.GlobalMode = isGlobalMode
	a.SetProjectContext(cfg.ProjectFilePath, cfg.ProjectGuidelines, cfg.TestCommand, cfg.LintCommand)
	a.TodoManager = agent.NewTodoManager(cfg.WorkingDir)
	a.PinManager = agent.NewPinManager()
	a.DebugCompareMessages = cfg.DebugCompareMessages
	// Feature flags: seeded from startup config (TUI/CLI path; the web
	// server re-seeds from its workspace on every NewSessionAgent and
	// supports live toggles via the config WS message).
	a.SetBoardEnabled(cfg.BoardEnabled())
	a.SetSubagentsEnabled(cfg.SubagentEnabled())
	a.SetSubagentMaxDepth(cfg.SubagentDepth())
	if cfg.BoardEnabled() {
		// Project board: per-process manager rooted at the working dir
		// (global mode → the global board dir). Re-pointed on /dir changes
		// via AfterWorkingDirChange.
		a.SetBoardManager(agent.NewBoardManager(cfg.WorkingDir, isGlobalMode))
	}
	if cfg.DebugCompareMessages && !agent.ViewDriftCompiledIn() {
		fmt.Fprintf(os.Stderr, "GOGEN_DEBUG_COMPARE_MESSAGES requires a debug build (-tags debug); ignoring\n")
		a.DebugCompareMessages = false
	}

	sessionEnabled := !strings.EqualFold(os.Getenv("GOGEN_SESSION_PERSIST"), "off")
	sessionOpts := session.StoreOptions{
		MaxCount:   cfg.SessionMaxCount,
		MaxAgeDays: cfg.SessionMaxAgeDays,
	}
	store := session.NewStoreWithOptions(sessionEnabled, sessionOpts)
	if isGlobalMode {
		// Use global session dir ~/.local/share/gogen/sessions/
		store.SetGlobalDir(projectfile.GlobalSessionDir())
	}
	a.SessionStore = store
	a.SessionID = session.NewID()
	// Local-only restore: avoid blocking startup on provider ListModels.
	var restoredModel string
	if sessionEnabled {
		if latest, err := store.LatestID(cfg.WorkingDir); err == nil && latest != "" {
			if snap, err := store.LoadInWorkingDir(cfg.WorkingDir, latest); err == nil {
				a.RestoreSession(snap, latest)
				restoredModel = snap.Model
				fmt.Fprintf(os.Stderr, "Session %s (%d msgs)\n", latest, len(a.Messages))
			}
		}
	}
	// One-time migration: adopt project-global todos.json into the current
	// session when it has no todos yet, then rename the legacy file.
	if a.ImportLegacyTodos() {
		fmt.Fprintf(os.Stderr, "Migrated legacy todos into session %s\n", a.SessionID)
	}
	if name := provider.ModelName(); name != "" {
		fmt.Fprintf(os.Stderr, "Model: %s\n", name)
	} else {
		fmt.Fprintf(os.Stderr, "No model selected; use /models to choose\n")
	}
	return a, restoredModel
}
