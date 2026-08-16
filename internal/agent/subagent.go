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
// schema exactly.
func subagentToolDef(bg bool) llm.Tool {
	desc := "Subagent tool: spawn a nested agent session to work on a job and report back. " +
		"The subagent runs independently with its own context window; its final " +
		"output is returned as the tool result. Use for isolated subtasks that " +
		"would otherwise bloat this conversation. Nesting follows subagent_max_depth " +
		"(default 1 = subagents cannot spawn subagents)."
	props := map[string]interface{}{
		"job":   toolProp("string", "The job prompt for the subagent (a complete, self-contained task)"),
		"model": toolProp("string", "Optional model override for the subagent"),
	}
	if bg {
		desc += " Set run_in_background=true for long-running work: the call returns " +
			"the subagent id immediately, the parent is notified when it finishes, " +
			"and while it runs it can be steered with send_message, interrupted with " +
			"interrupt_agent, and resolved with list_agents."
		props["run_in_background"] = toolProp("boolean", "Run the subagent in the background and return its id immediately (default false)")
	}
	return toolDef("subagent", desc,
		toolSchema(props, []string{"job"}...),
	)
}

// subagentForkToolDef is the schema for the fork tool: a child seeded with
// a deep copy of this session's history, then one foreground turn.
func subagentForkToolDef() llm.Tool {
	return toolDef("subagent_fork",
		"Fork tool: create a child session seeded with a deep copy of THIS session's full history, "+
			"then run a job on it and report back. The fork starts with the same context as this conversation, "+
			"but its edits and turns happen only in the child — this transcript is untouched. "+
			"Use to explore an alternative approach without losing the current thread, or to delegate "+
			"a continuation of this task. Optional job overrides the continuation prompt "+
			"(default: \"Continue this session from the fork point.\"). Foreground: the call returns the fork's final output.",
		toolSchema(map[string]interface{}{
			"job": toolProp("string", "Optional job for the fork (default: continue this session from the fork point)"),
		}),
	)
}

// listAgentsToolDef is the schema for the read-only live-child listing.
func listAgentsToolDef() llm.Tool {
	return toolDef("list_agents",
		"List the live nested (subagent) sessions of this session: id, label, status (running / idle / finished), "+
			"and nesting depth. Read-only. Use to resolve an agent id before send_message / interrupt_agent, "+
			"or to check whether a background subagent is still working. Only live in-memory children are listed; "+
			"finished children stay listed until their retention window elapses.",
		toolSchema(map[string]interface{}{}),
	)
}

// sendMessageToolDef is the schema for steering a background subagent.
func sendMessageToolDef() llm.Tool {
	return toolDef("send_message",
		"Send a message to a running background subagent (agent_id). The message is delivered to the subagent's "+
			"session as a new user message and starts a turn; if the subagent is mid-turn, the message is queued "+
			"and delivered when it becomes idle. The subagent's response is injected back into this session as a "+
			"user message when it finishes replying. Use to steer a background subagent mid-task: new instructions, "+
			"changed requirements, or a stop-and-report request.",
		toolSchema(map[string]interface{}{
			"agent_id": toolProp("string", "The subagent id (see list_agents)"),
			"message":  toolProp("string", "The message to deliver to the subagent"),
		}, "agent_id", "message"),
	)
}

// interruptAgentToolDef is the schema for cancelling a child's in-flight
// turn without killing the child session.
func interruptAgentToolDef() llm.Tool {
	return toolDef("interrupt_agent",
		"Cancel a running subagent's in-flight turn (agent_id). The subagent session stays alive — only its "+
			"current turn stops; it can be messaged again or left idle. Use when a background subagent is stuck, "+
			"looping, or working on the wrong thing. No-op when the subagent is idle.",
		toolSchema(map[string]interface{}{
			"agent_id": toolProp("string", "The subagent id (see list_agents)"),
		}, "agent_id"),
	)
}

// reportToolDef is the schema for the child-scoped progress report. It is
// exposed ONLY to agents that are themselves live nested children with an
// installed report hook.
func reportToolDef() llm.Tool {
	return toolDef("report",
		"Report progress to the parent session: injects this subagent's message as a user message into the "+
			"parent conversation and starts a turn there (queued if the parent is busy). Use to surface "+
			"intermediate findings, hand back partial results before finishing, or ask the parent for input. "+
			"Available only inside a continuable subagent.",
		toolSchema(map[string]interface{}{
			"message": toolProp("string", "The progress message to deliver to the parent session"),
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
func handleSubagentFork(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
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
func handleListAgents(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("list_agents is not supported in this mode")
	}
	return cs.ListAgents(a)
}

// handleSendMessage delivers a message to a live background subagent.
func handleSendMessage(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
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
func handleInterruptAgent(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
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
func handleReport(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	message, err := stringArg(args, "message")
	if err != nil {
		return "", err
	}
	if err := a.reportToParent(message); err != nil {
		return "", err
	}
	return "Reported to the parent session.", nil
}
