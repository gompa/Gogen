package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// SubagentLabel derives the child session's sidebar title from the job.
// Shared by the web and TUI spawners so labels cannot drift between modes.
func SubagentLabel(job string) string {
	first := job
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	if len(first) > 60 {
		first = first[:60] + "…"
	}
	if first == "" {
		return "subagent"
	}
	return "subagent: " + first
}

// MaxSubagentReportBytes caps the report returned to the parent tool call
// (mirrors the general tool-result truncation spirit; the child's full
// transcript stays inspectable via its pane).
const MaxSubagentReportBytes = 64 * 1024

// TruncateSubagentReport bounds a subagent's final report to
// MaxSubagentReportBytes. Shared by the web and TUI spawners.
func TruncateSubagentReport(report string) string {
	if len(report) > MaxSubagentReportBytes {
		return report[:MaxSubagentReportBytes] + "… (truncated)"
	}
	return report
}

// SubagentSpawner runs a nested session with a job prompt and returns its
// final report. Installed by the web server (workspace + registry) and the
// TUI runner; nil means the subagent tool is unavailable ("spawner not
// installed") — the same nil-guard contract as the MCP registry.
type SubagentSpawner interface {
	// Spawn runs a nested agent turn for job (a full prompt). parent is the
	// agent that issued the tool call (the spawner uses it to resolve the
	// parent runtime for client broadcasts and approval routing). depth is
	// the child's nesting depth (parent = 0, child = 1, ...). model
	// overrides the child's model when non-empty. Returns the child's final
	// response.
	Spawn(ctx context.Context, parent *Agent, job, model string, depth int) (string, error)
}

// subagentToolDef is the LLM-facing schema for the subagent tool. Like the
// board tool it lives OUTSIDE builtinToolDefs (MCP-style gating): exposed
// only when the subagent feature is enabled AND a spawner is installed.
func subagentToolDef() llm.Tool {
	return toolDef("subagent",
		"Subagent tool: spawn a nested agent session to work on a job and report back. "+
			"The subagent runs independently (its own context window) and its final "+
			"output is returned as the tool result. Use for isolated subtasks that "+
			"would otherwise bloat this conversation. Subagents cannot spawn subagents.",
		toolSchema(
			map[string]interface{}{
				"job":   toolProp("string", "The job prompt for the subagent (a complete, self-contained task)"),
				"model": toolProp("string", "Optional model override for the subagent"),
			},
			[]string{"job"}...,
		),
	)
}

// handleSubagent executes a subagent tool call. Reachable only when the
// subagent feature is enabled (all tool surfaces gate on SubagentsEnabled),
// so the spawner nil-guard is the only check needed here.
func handleSubagent(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	spawner := a.SubagentSpawner()
	if spawner == nil {
		return "", fmt.Errorf("subagent spawner is not installed (subagents are unavailable in this mode)")
	}
	job, err := stringArg(args, "job")
	if err != nil {
		return "", err
	}
	model, _ := stringArgOptional(args, "model")
	if a.SubagentDepth() >= a.SubagentMaxDepth() {
		return "", fmt.Errorf("subagent nesting depth limit reached (%d of %d): this subagent cannot spawn further subagents",
			a.SubagentDepth(), a.SubagentMaxDepth())
	}
	return spawner.Spawn(ctx, a, job, model, a.SubagentDepth())
}
