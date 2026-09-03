package agent

import (
	"sync"
	"sync/atomic"

	"gogen/internal/config"
	"gogen/internal/skills"
)

// FeatureFlags is the single live store for the board / subagent feature
// flags. A workspace owns one instance and every session agent it spawns
// reads the SAME instance (Agent.SetFeatureFlags), so a settings toggle is
// visible to all sessions immediately — no per-agent mirror and no sweep.
//
// All values are atomic: the web settings toggle writes them while
// per-turn tool derivation (llmTools/AllowedToolNames/executeTool) and the
// subagent spawner read them concurrently.
type FeatureFlags struct {
	boardEnabled          atomic.Bool
	subagentsEnabled      atomic.Bool
	subagentMaxDepth      atomic.Int32
	subagentMaxConcurrent atomic.Int32
}

// NewFeatureFlags creates a FeatureFlags store seeded with the given
// values. maxDepth/maxConcurrent <= 0 mean "unset" — readers apply the
// config defaults (see Agent.SubagentMaxDepth / SubagentMaxConcurrent).
func NewFeatureFlags(boardEnabled, subagentsEnabled bool, maxDepth, maxConcurrent int) *FeatureFlags {
	f := &FeatureFlags{}
	f.boardEnabled.Store(boardEnabled)
	f.subagentsEnabled.Store(subagentsEnabled)
	f.subagentMaxDepth.Store(int32(maxDepth))
	f.subagentMaxConcurrent.Store(int32(maxConcurrent))
	return f
}

// BoardEnabled reports whether the project kanban board feature is active.
func (f *FeatureFlags) BoardEnabled() bool {
	return f.boardEnabled.Load()
}

// SetBoardEnabled toggles the board feature.
func (f *FeatureFlags) SetBoardEnabled(on bool) {
	f.boardEnabled.Store(on)
}

// SubagentsEnabled reports whether the subagent feature is active.
func (f *FeatureFlags) SubagentsEnabled() bool {
	return f.subagentsEnabled.Load()
}

// SetSubagentsEnabled toggles the subagent feature.
func (f *FeatureFlags) SetSubagentsEnabled(on bool) {
	f.subagentsEnabled.Store(on)
}

// SubagentMaxDepth returns the stored nesting-depth limit (0 = unset).
func (f *FeatureFlags) SubagentMaxDepth() int {
	return int(f.subagentMaxDepth.Load())
}

// SetSubagentMaxDepth stores the nesting-depth limit (0 = unset).
func (f *FeatureFlags) SetSubagentMaxDepth(depth int) {
	f.subagentMaxDepth.Store(int32(depth))
}

// SubagentMaxConcurrent returns the stored per-parent concurrent-subagent
// limit (0 = unset).
func (f *FeatureFlags) SubagentMaxConcurrent() int {
	return int(f.subagentMaxConcurrent.Load())
}

// SetSubagentMaxConcurrent stores the per-parent concurrent-subagent limit
// (0 = unset).
func (f *FeatureFlags) SetSubagentMaxConcurrent(n int) {
	f.subagentMaxConcurrent.Store(int32(n))
}

// flags returns this agent's feature-flag store, lazily creating a private
// instance for agents that were never pointed at a shared one (standalone
// TUI/CLI agents, bare &Agent{} test literals).
func (a *Agent) flags() *FeatureFlags {
	if f := a.featureFlags.Load(); f != nil {
		return f
	}
	f := NewFeatureFlags(false, false, 0, 0)
	if !a.featureFlags.CompareAndSwap(nil, f) {
		f = a.featureFlags.Load()
	}
	return f
}

// SetFeatureFlags points this agent at a shared FeatureFlags store (the
// workspace's instance in web mode). From then on the agent's flag
// getters/setters read and write the shared state, so a workspace toggle is
// visible to this agent immediately — no per-agent mirror, no sweep.
// Passing nil detaches (the agent falls back to a lazily-created private
// store).
func (a *Agent) SetFeatureFlags(f *FeatureFlags) {
	a.featureFlags.Store(f)
}

// FeatureFlags returns the effective flag store this agent reads: the
// shared instance when one was attached, otherwise the lazily-created
// private one.
func (a *Agent) FeatureFlags() *FeatureFlags {
	return a.flags()
}

// SetBoardEnabled toggles the project kanban board feature for this agent.
// The web server publishes live toggles to the shared store; per-turn tool
// derivation (llmTools/AllowedToolNames) reads the value atomically.
func (a *Agent) SetBoardEnabled(on bool) {
	a.flags().SetBoardEnabled(on)
}

// BoardEnabled reports whether the board tool is registered for this agent.
func (a *Agent) BoardEnabled() bool {
	return a.flags().BoardEnabled()
}

// SetSubagentsEnabled toggles the subagent tool for this agent (see
// SetBoardEnabled for the live-toggle contract).
func (a *Agent) SetSubagentsEnabled(on bool) {
	a.flags().SetSubagentsEnabled(on)
}

// SubagentsEnabled reports whether the subagent tool is registered for this
// agent.
func (a *Agent) SubagentsEnabled() bool {
	return a.flags().SubagentsEnabled()
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
	a.flags().SetSubagentMaxDepth(depth)
}

// SubagentMaxDepth returns the effective maximum subagent nesting depth.
func (a *Agent) SubagentMaxDepth() int {
	if d := a.flags().SubagentMaxDepth(); d > 0 {
		return d
	}
	return config.DefaultSubagentMaxDepth
}

// SetSubagentMaxConcurrent sets the maximum number of subagents that may
// run concurrently per parent session. Values <= 0 fall back to the config
// default.
func (a *Agent) SetSubagentMaxConcurrent(n int) {
	a.flags().SetSubagentMaxConcurrent(n)
}

// SubagentMaxConcurrent returns the effective per-parent concurrent-subagent
// limit.
func (a *Agent) SubagentMaxConcurrent() int {
	if n := a.flags().SubagentMaxConcurrent(); n > 0 {
		return n
	}
	return config.DefaultSubagentMaxConcurrent
}

// featureWiring groups the shared feature managers (feature flags, board,
// skills, workspace instructions) and the host callbacks the web server
// installs on every session agent. See the featureFlags comment for the
// shared-store contract.
type featureWiring struct {
	// Feature flags, live-toggleable from the web settings modal (config WS
	// message). featureFlags points at the SHARED FeatureFlags store: in web
	// mode every session agent reads the workspace's single instance (set
	// via SetFeatureFlags at spawn time), so a settings toggle is visible to
	// all sessions with no per-agent mirror and no sweep. Standalone agents
	// (TUI/CLI, bare &Agent{} test literals) lazily get a private instance
	// on first access. The atomic stores inside publish toggles to every
	// concurrent reader: llmTools/AllowedToolNames consult
	// BoardEnabled/SubagentsEnabled per turn, and the toggle handler rebuilds
	// toolHandlers under toolMu so executeTool's map lookup is the gate.
	// SubagentMaxDepth bounds nesting (main agent = depth 0; default 1 =
	// subagents cannot spawn subagents) and SubagentMaxConcurrent bounds
	// per-parent concurrent subagents (default
	// config.DefaultSubagentMaxConcurrent).
	featureFlags atomic.Pointer[FeatureFlags]

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
}
