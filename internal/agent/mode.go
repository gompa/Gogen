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

// allowsTool reports whether name may run in the agent's current mode,
// covering the static builtin set plus the dynamic feature tools. D7: the
// board tool is plan-mode unrestricted (the coordination exception, like
// todo) — an agent may update the board in plan mode so it can mark items
// for review.
func (a *Agent) allowsTool(name string) bool {
	if a.Mode != ModePlan {
		return true
	}
	if _, ok := planModeAllowedTools[name]; ok {
		return true
	}
	if name == "board" && a.BoardEnabled() && a.BoardManager() != nil {
		return true
	}
	// D-skill: skill list/read are read-only, so the skill tool is
	// plan-mode allowed like the board (the model can consult skills while
	// planning).
	if name == "skill" && a.SkillsEnabled() && a.SkillsManager() != nil {
		return true
	}
	return false
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
	if a.Mode == ModePlan && !a.allowsTool(toolName) {
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
		if a.allowsTool(name) {
			out[name] = struct{}{}
		}
	}
	// Feature tools: the board tool is available in both modes (D7), the
	// subagent tool only in act mode.
	if a.BoardEnabled() && a.BoardManager() != nil && a.allowsTool("board") {
		out["board"] = struct{}{}
	}
	if a.SubagentsEnabled() && a.SubagentSpawner() != nil && a.Mode != ModePlan {
		out["subagent"] = struct{}{}
		if a.continuableSpawner() != nil {
			out["subagent_fork"] = struct{}{}
			out["list_agents"] = struct{}{}
			out["send_message"] = struct{}{}
			out["interrupt_agent"] = struct{}{}
			if a.ParentID() != "" && a.ReportHook() != nil {
				out["report"] = struct{}{}
			}
		}
	}
	if a.SkillsEnabled() && a.SkillsManager() != nil && a.allowsTool("skill") {
		out["skill"] = struct{}{}
	}
	if a.Mode != ModePlan && a.MCPRegistry != nil {
		for name := range a.MCPRegistry.ToolNames() {
			out[name] = struct{}{}
		}
	}
	return out
}
