package agent

import (
	"context"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// llmTools returns the tool definitions exposed to the model: the built-in
// tools plus any registered MCP server tools.
func (a *Agent) llmTools() []llm.Tool {
	tools := BuiltinTools()
	if a.MCPRegistry != nil {
		tools = append(tools, a.MCPRegistry.Definitions()...)
	}
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
			if a.shouldCompactUsingCounts() {
				if h != nil && h.OnCompacting != nil {
					h.OnCompacting()
				}
				var pinned map[int]struct{}
				if a.PinManager != nil {
					pinned = a.PinManager.PinnedSet()
				}
				compacted, newPins, err := a.Context.CompactPinned(ctx, a.systemPromptPrefix(), a.Messages, pinned)
				if err == nil {
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

	view = buildSystemView(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)

	a.recordViewForDrift(view)
	return view
}

// systemPromptPrefix returns the system/enrichment messages that precede
// canonical history on the wire (the view minus a.Messages). CompactPinned
// prepends these to the summarization request so the conversation prefix is
// byte-identical to the previous turn and provider prompt caching applies.
func (a *Agent) systemPromptPrefix() []llm.Message {
	if len(a.Messages) == 0 {
		return nil
	}
	view := buildSystemView(a.Messages, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.ensureProjectProfile(), a.Mode)
	return view[:len(view)-len(a.Messages)]
}
