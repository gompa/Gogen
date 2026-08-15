package agent

import (
	"context"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// llmTools returns the tool definitions exposed to the model: the built-in
// tools, the feature-gated board tool when enabled, plus any registered MCP
// server tools. Feature tools are appended conditionally (MCP-style) so they
// have zero registry trace when their feature is off.
//
// A registered MCP tool whose name collides with a builtin or feature tool
// SHADOWS it here (its definition is the one the model sees): executeTool
// prefers the registry on name collisions too, so what the model sees and
// what actually executes always agree — and the model never sees duplicate
// definitions for one name (several APIs reject those).
func (a *Agent) llmTools() []llm.Tool {
	var mcpDefs []llm.Tool
	mcpNames := make(map[string]struct{})
	if a.MCPRegistry != nil {
		mcpDefs = a.MCPRegistry.Definitions()
		for _, t := range mcpDefs {
			mcpNames[t.Name] = struct{}{}
		}
	}
	tools := make([]llm.Tool, 0, len(BuiltinTools())+2+len(mcpDefs))
	for _, t := range BuiltinTools() {
		if _, shadowed := mcpNames[t.Name]; shadowed {
			continue // the MCP definition is appended below
		}
		tools = append(tools, t)
	}
	if a.BoardEnabled() && a.BoardManager() != nil {
		if _, shadowed := mcpNames["board"]; !shadowed {
			tools = append(tools, boardToolDef())
		}
	}
	if a.SubagentsEnabled() && a.SubagentSpawner() != nil {
		cs := a.continuableSpawner()
		shadowed := map[string]bool{}
		for _, name := range []string{"subagent", "subagent_fork", "list_agents", "send_message", "interrupt_agent", "report"} {
			if _, ok := mcpNames[name]; ok {
				shadowed[name] = true
			}
		}
		if !shadowed["subagent"] {
			tools = append(tools, subagentToolDef(cs != nil))
		}
		if cs != nil {
			if !shadowed["subagent_fork"] {
				tools = append(tools, subagentForkToolDef())
			}
			if !shadowed["list_agents"] {
				tools = append(tools, listAgentsToolDef())
			}
			if !shadowed["send_message"] {
				tools = append(tools, sendMessageToolDef())
			}
			if !shadowed["interrupt_agent"] {
				tools = append(tools, interruptAgentToolDef())
			}
			// report is child-scoped: only nested agents with an installed
			// report hook see it (a restored subagent session reopened by a
			// user has ParentID but no hook, so it stays hidden).
			if a.ParentID() != "" && a.ReportHook() != nil && !shadowed["report"] {
				tools = append(tools, reportToolDef())
			}
		}
	}
	if a.SkillsEnabled() && a.SkillsManager() != nil {
		if _, shadowed := mcpNames["skill"]; !shadowed {
			tools = append(tools, skillsToolDef())
		}
	}
	tools = append(tools, mcpDefs...)
	return tools
}

// prepareMessages builds the LLM view for the next round, compacting history
// at conversation boundaries when auto-compaction triggers. h carries the
// stream handlers for the round; OnCompacting fires before the summarization
// call so the UI can show compaction progress.
func (a *Agent) prepareMessages(ctx context.Context, h *llm.StreamHandlers) []llm.Message {
	var view []llm.Message
	if a.Context == nil {
		view = a.Messages
	} else {
		a.Context.EnsureContextLimit(ctx)
		// Only compact at conversation boundaries (when the
		// most recent message is from the user).  Compacting
		// mid-tool-loop can drop assistant tool-call messages
		// whose results are still pending, confusing the LLM.
		if len(a.Messages) > 0 && a.Messages[len(a.Messages)-1].Role == "user" {
			if a.shouldCompactUsingCounts() && a.compactAttemptDue() {
				if h != nil && h.OnCompacting != nil {
					h.OnCompacting()
				}
				var pinned map[int]struct{}
				if a.PinManager != nil {
					pinned = a.PinManager.PinnedSet()
				}
				// Pass the cached per-message counts so the summarization
				// request can be sized without re-tokenizing the middle.
				a.statsMu.RLock()
				counts := append([]int(nil), a.tokenCounts...)
				a.statsMu.RUnlock()
				compacted, newPins, err := a.Context.CompactPinned(ctx, a.systemPromptPrefix(), a.Messages, counts, pinned)
				if err != nil {
					// A failing summarization call must not be retried on every turn.
					a.noteCompactFailure(err)
				} else {
					// Compute the post-compaction counts BEFORE publishing —
					// the conversation just shrank, so counting it is cheap —
					// and publish them atomically. A nil cache here would
					// make the next turn's shouldCompactUsingCounts fall back
					// to a full EstimateTokens pass (the cache is otherwise
					// only backfilled by ContextStats/doPersist).
					counts := make([]int, len(compacted))
					for i, m := range compacted {
						counts[i] = contextmgr.ComputeMessageTokens(m)
					}
					a.replaceMessagesWithCounts(compacted, counts)
					if a.PinManager != nil {
						a.PinManager.ReplacePins(newPins)
					}
					// lastTurnUsage is no longer representative after compaction.
					a.clearTurnUsage()
					a.resetSaveTracking()
					a.noteCompactSuccess()
				}
			}
		}
		// EnsureToolResultsCapped rewrites oversized tool bodies in place on
		// the live message array; exclude concurrent ContextStats clones.
		a.statsMu.Lock()
		if a.Context.EnsureToolResultsCapped(a.Messages) {
			// Tool bodies were rewritten in place, so cached counts for the
			// affected messages are stale. Drop the cache; it is rebuilt on
			// the next ContextStats/save.
			a.tokenCounts = nil
		}
		a.statsMu.Unlock()
		view = a.Messages
	}
	// Stabilize tool args on a.Messages (not view, which may be a copy) so
	// ArgsStabilized is persisted and we skip already-stable messages.
	a.stabilizeToolArgs()

	view = buildSystemView(view, a.WorkingDir, a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode)

	a.recordViewForDrift(view)
	return view
}

// systemPromptPrefix returns the system/enrichment messages that precede
// canonical history on the wire (the view minus a.Messages). CompactPinned
// prepends these to the summarization request so the conversation prefix is
// byte-identical to the previous turn and provider prompt caching applies.
// Built without copying the history: the prefix is either empty (the history
// already carries a system message that buildSystemView enriches in place)
// or the single prepended system message.
func (a *Agent) systemPromptPrefix() []llm.Message {
	if len(a.Messages) == 0 {
		return nil
	}
	for _, m := range a.Messages {
		if m.Role == "system" {
			return nil
		}
	}
	return []llm.Message{{
		Role: "system",
		Content: SystemPrompt(a.WorkingDir) +
			buildSystemSuffix(a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode),
	}}
}
