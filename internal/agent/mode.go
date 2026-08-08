package agent

import (
	"fmt"
	"strings"
)

// Mode controls whether the agent may mutate the repository.
type Mode int

const (
	ModeAct Mode = iota
	ModePlan
)

// ErrPlanModeBlocked is returned when a tool is disabled in plan mode.
var ErrPlanModeBlocked = fmt.Errorf("plan mode blocked tool")

// planModeAllowedTools is the intentional read-mostly subset for plan mode,
// derived from the PlanAllowed flag on each builtin tool's ToolDef so the
// registry stays the single source of truth. Mutating tools from
// BuiltinTools() are excluded by omission (no PlanAllowed flag).
var planModeAllowedTools = derivePlanModeAllowedTools()

func derivePlanModeAllowedTools() map[string]struct{} {
	out := make(map[string]struct{}, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		if d.PlanAllowed {
			out[d.Definition.Name] = struct{}{}
		}
	}
	return out
}

// builtinToolNames is derived from BuiltinTools() so schema and allowlists stay in sync.
var builtinToolNames = deriveBuiltinToolNames()

func deriveBuiltinToolNames() []string {
	tools := BuiltinTools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func (m Mode) String() string {
	if m == ModePlan {
		return "plan"
	}
	return "act"
}

// ParseMode parses act/plan strings.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "plan", "Plan", "PLAN":
		return ModePlan, true
	case "act", "Act", "ACT", "":
		return ModeAct, true
	default:
		return ModeAct, false
	}
}

// AllowsTool reports whether the tool may run in this mode.
func (m Mode) AllowsTool(name string) bool {
	if m != ModePlan {
		return true
	}
	_, ok := planModeAllowedTools[name]
	return ok
}

func (a *Agent) SetMode(m Mode) {
	// Written under statsMu so config snapshots (agentConfigMsgBasic) can
	// read the mode WITHOUT the session turn lock — the web attach handshake
	// must never block on a running turn, which holds turnMu for its entire
	// duration.
	a.statsMu.Lock()
	a.Mode = m
	a.statsMu.Unlock()
	a.FlushSession()
}

// ModeAndThinkingLevel returns the current mode and thinking level under
// statsMu. Safe to call without holding turnMu; used by the web server's
// config snapshot so a mid-turn attach never blocks on the turn lock.
func (a *Agent) ModeAndThinkingLevel() (Mode, ThinkingLevel) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.Mode, a.ThinkingLevel
}

func (a *Agent) checkPlanMode(toolName string) error {
	if a.Mode == ModePlan && !a.Mode.AllowsTool(toolName) {
		return fmt.Errorf("%w: tool %q is disabled; use /act to implement changes", ErrPlanModeBlocked, toolName)
	}
	if a.Mode == ModePlan && a.isMCPTool(toolName) {
		return fmt.Errorf("%w: MCP tool %q is disabled in plan mode", ErrPlanModeBlocked, toolName)
	}
	return nil
}

func (a *Agent) isMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp_")
}

// AllowedToolNames returns tool names available to the LLM in the current mode.
func (a *Agent) AllowedToolNames() map[string]struct{} {
	out := make(map[string]struct{})
	for _, name := range builtinToolNames {
		if a.Mode.AllowsTool(name) {
			out[name] = struct{}{}
		}
	}
	if a.Mode != ModePlan && a.MCPRegistry != nil {
		for name := range a.MCPRegistry.ToolNames() {
			out[name] = struct{}{}
		}
	}
	return out
}
