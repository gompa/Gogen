package agent

import "fmt"

// Mode controls whether the agent may mutate the repository.
type Mode int

const (
	ModeAct Mode = iota
	ModePlan
)

// ErrPlanModeBlocked is returned when a tool is disabled in plan mode.
var ErrPlanModeBlocked = fmt.Errorf("plan mode blocked tool")

// planModeAllowedTools is the intentional read-mostly subset for plan mode.
// Mutating tools from BuiltinTools() are excluded by omission.
var planModeAllowedTools = map[string]struct{}{
	"repo_overview":    {},
	"list_files":       {},
	"glob_files":       {},
	"read_file":        {},
	"read_files":       {},
	"list_definitions": {},
	"search_code":      {},
	"find_references":  {},
	"show_diff":        {},
	"git_log":          {},
	"git_blame":        {},
	"git_status":       {},
	// git_branch omitted: create/switch mutate the repo; list via execute outside plan.
	"git_stash_list":   {},
	"git_show":         {},
	"web_search":       {},
	"web_fetch":        {},
	"find_file":        {},
	"find_definition":  {},
	"todo_add":         {},
	"todo_list":        {},
	"session_rename":   {},
	"session_usage":    {},
	"context_pin_last": {},
	"context_pins":     {},
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
	a.Mode = m
	a.FlushSession()
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
	return len(name) > 4 && name[:4] == "mcp_"
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
