package agent

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"

	"gogen/internal/contextmgr"
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
	// Rune-safe cut: slicing on bytes could split a multi-byte character
	// and inject invalid UTF-8 into the sidebar label.
	if r := []rune(first); len(r) > 60 {
		first = string(r[:60]) + "…"
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
	return contextmgr.TruncateMarked(report, MaxSubagentReportBytes, "… (truncated)")
}

// ApplySubagentThinkingLevel resolves and applies the child's reasoning
// effort: the configured subagent level (the subagent_thinking_level
// setting) when non-empty, otherwise the parent's live level (empty =
// inherit). "off" and empty are the child's default and set nothing. A
// level the child's FINAL model does not accept is omitted (policy B) and
// logged — the tool's explicit model argument can override the configured
// subagent model, so validity is resolved against the child's model at
// spawn time, never at save time. Shared by the web and TUI spawners so
// the two cannot drift (like SubagentLabel).
func ApplySubagentThinkingLevel(child, parent *Agent, configured string) {
	level := NormalizeThinkingLevel(configured)
	if level == "" {
		_, level = parent.ModeAndThinkingLevel()
	}
	if level == "" || level == ThinkingOff {
		return
	}
	if !slices.Contains(child.CurrentModelEfforts(), string(level)) {
		log.Printf("subagent: thinking level %q not accepted by child model %q; omitted", level, child.CurrentModel())
		return
	}
	child.SetThinkingLevel(level)
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

// ContinuableSubagentSpawner is the optional extension of SubagentSpawner
// for background/continuable subagents (web host; the TUI stays foreground
// in v1). When the installed spawner implements it, the subagent tool gains
// run_in_background and the control tools (subagent_fork, list_agents,
// send_message, interrupt_agent) are registered; report is additionally
// child-scoped via the agent's report hook.
type ContinuableSubagentSpawner interface {
	// SpawnBackground starts a nested session that keeps running after this
	// call returns. The parent is notified (delivery service) when the
	// child finishes; while it lives it can be messaged (SendMessage),
	// interrupted (InterruptAgent), and listed (ListAgents).
	SpawnBackground(ctx context.Context, parent *Agent, job, model string, depth int) (id string, err error)
	// Fork seeds a child with a deep copy of the parent's messages and runs
	// one foreground turn. job may be empty for the default continuation
	// prompt. Returns the fork's final output.
	Fork(ctx context.Context, parent *Agent, job string, depth int) (string, error)
	// ListAgents lists the caller's live children (id, label, status,
	// depth) as a human-readable listing.
	ListAgents(caller *Agent) (string, error)
	// SendMessage delivers text to a live child of caller as a new user
	// message; queued when the child is mid-turn.
	SendMessage(caller *Agent, agentID, text string) error
	// InterruptAgent cancels a live child's in-flight turn (no-op when
	// idle). The child session stays alive.
	InterruptAgent(caller *Agent, agentID string) error
}

// subagentToolDef is the LLM-facing schema for the subagent tool. Like the
// board tool it lives OUTSIDE builtinToolDefs (MCP-style gating): exposed
// only when the subagent feature is enabled AND a spawner is installed.
// bg=true (the installed spawner implements ContinuableSubagentSpawner)
// adds the run_in_background parameter; TUI v1 keeps the foreground-only
// schema exactly. maxConcurrent is the live per-parent concurrent-subagent
// limit (subagent_max_concurrent) surfaced in the description so the model
// knows how many background children it can run at once.
func subagentToolDef(bg bool, maxConcurrent int) llm.Tool {
	desc := "Spawn a child agent on an isolated task; its final output returns as the tool result. " +
		"Use for subtasks that would otherwise bloat this context. Subagents cannot " +
		"spawn subagents by default. " +
		fmt.Sprintf("At most %d may run concurrently.", maxConcurrent)
	props := map[string]any{
		"job":   toolProp("string", "Complete, self-contained task for the child agent"),
		"model": toolProp("string", "Optional model override"),
	}
	if bg {
		desc += " run_in_background=true returns the child id immediately; the parent is " +
			"notified on completion. Steer with send_message, interrupt with " +
			"interrupt_agent, check with list_agents."
		props["run_in_background"] = toolProp("boolean", "Run in background; returns the child id immediately")
	}
	return toolDef("subagent", desc,
		toolSchema(props, []string{"job"}...),
	)
}

// subagentForkToolDef is the schema for the fork tool: a child seeded with
// a deep copy of this session's history, then one foreground turn.
func subagentForkToolDef() llm.Tool {
	return toolDef("subagent_fork",
		"Fork this session: the child starts with this transcript and runs a job; its edits and "+
			"turns stay in the child, this conversation is untouched. Use to explore an alternative "+
			"approach or continue this task without blocking. Foreground: returns the fork's final output.",
		toolSchema(map[string]any{
			"job": toolProp("string", "Optional job (default: continue this session)"),
		}),
	)
}

// listAgentsToolDef is the schema for the read-only live-child listing.
func listAgentsToolDef() llm.Tool {
	return toolDef("list_agents",
		"List this session's child subagents: id, label, status (running/idle/finished), depth. "+
			"Read-only. Use to resolve agent ids for send_message/interrupt_agent or check background "+
			"status. Finished children remain listed briefly.",
		toolSchema(map[string]any{}),
	)
}

// sendMessageToolDef is the schema for steering a background subagent.
func sendMessageToolDef() llm.Tool {
	return toolDef("send_message",
		"Send a message to a running background subagent (agent_id); queued if it is mid-turn. "+
			"Its reply is injected into this session when it finishes. Use to steer a background subagent: "+
			"new instructions, changed requirements, or stop-and-report.",
		toolSchema(map[string]any{
			"agent_id": toolProp("string", "Subagent id (from list_agents)"),
			"message":  toolProp("string", "The message to deliver to the subagent"),
		}, "agent_id", "message"),
	)
}

// interruptAgentToolDef is the schema for cancelling a child's in-flight
// turn without killing the child session.
func interruptAgentToolDef() llm.Tool {
	return toolDef("interrupt_agent",
		"Cancel a subagent's in-flight turn (agent_id); the session stays alive and can be messaged again. "+
			"No-op when idle. Use when a background subagent is stuck or off-track.",
		toolSchema(map[string]any{
			"agent_id": toolProp("string", "Subagent id (from list_agents)"),
		}, "agent_id"),
	)
}

// reportToolDef is the schema for the child-scoped progress report. It is
// exposed ONLY to agents that are themselves live nested children with an
// installed report hook.
func reportToolDef() llm.Tool {
	return toolDef("report",
		"Send a progress update to the parent session (starts a turn there; queued if busy). Use to "+
			"surface findings, hand back partial results, or ask the parent for input. Child-only.",
		toolSchema(map[string]any{
			"message": toolProp("string", "Progress update for the parent"),
		}, "message"),
	)
}

// continuable returns the installed spawner as a ContinuableSubagentSpawner
// when it supports background/continuable subagents.
func (a *Agent) continuableSpawner() ContinuableSubagentSpawner {
	cs, _ := a.SubagentSpawner().(ContinuableSubagentSpawner)
	return cs
}

// handleSubagent executes a subagent tool call. Reachable only when the
// subagent feature is enabled (all tool surfaces gate on SubagentsEnabled),
// so the spawner nil-guard is the only check needed here.
func handleSubagent(ctx context.Context, a *Agent, args map[string]any) (string, error) {
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
	bg, err := boolArg(args, "run_in_background", false)
	if err != nil {
		return "", err
	}
	if bg {
		cs := a.continuableSpawner()
		if cs == nil {
			return "", fmt.Errorf("background subagents are not supported in this mode")
		}
		id, err := cs.SpawnBackground(ctx, a, job, model, a.SubagentDepth())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Subagent %s started in background (label: %s). "+
			"You will be notified when it finishes; use list_agents to check on it, send_message to steer it, "+
			"and interrupt_agent to stop a turn.", id, SubagentLabel(job)), nil
	}
	return spawner.Spawn(ctx, a, job, model, a.SubagentDepth())
}

// handleSubagentFork executes the fork tool (foreground). Reachable only
// when the subagent feature is enabled and the spawner is continuable.
func handleSubagentFork(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("subagent_fork is not supported in this mode")
	}
	job, _ := stringArgOptional(args, "job")
	if a.SubagentDepth() >= a.SubagentMaxDepth() {
		return "", fmt.Errorf("subagent nesting depth limit reached (%d of %d): this subagent cannot spawn further subagents",
			a.SubagentDepth(), a.SubagentMaxDepth())
	}
	return cs.Fork(ctx, a, job, a.SubagentDepth())
}

// handleListAgents lists the caller's live children (read-only).
func handleListAgents(_ context.Context, a *Agent, _ map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("list_agents is not supported in this mode")
	}
	return cs.ListAgents(a)
}

// handleSendMessage delivers a message to a live background subagent.
func handleSendMessage(_ context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("send_message is not supported in this mode")
	}
	agentID, err := stringArg(args, "agent_id")
	if err != nil {
		return "", err
	}
	message, err := stringArg(args, "message")
	if err != nil {
		return "", err
	}
	if err := cs.SendMessage(a, agentID, message); err != nil {
		return "", err
	}
	return fmt.Sprintf("Message delivered to subagent %s.", agentID), nil
}

// handleInterruptAgent cancels a subagent's in-flight turn.
func handleInterruptAgent(_ context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("interrupt_agent is not supported in this mode")
	}
	agentID, err := stringArg(args, "agent_id")
	if err != nil {
		return "", err
	}
	if err := cs.InterruptAgent(a, agentID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Interrupted subagent %s.", agentID), nil
}

// handleReport delivers a progress message from a child to its live parent.
// Reachable only for nested children with an installed report hook (all
// tool surfaces gate on that), so the hook nil-guard is the only check.
func handleReport(_ context.Context, a *Agent, args map[string]any) (string, error) {
	message, err := stringArg(args, "message")
	if err != nil {
		return "", err
	}
	if err := a.reportToParent(message); err != nil {
		return "", err
	}
	return "Reported to the parent session.", nil
}
