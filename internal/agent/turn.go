package agent

import (
	"context"

	"gogen/internal/llm"
)

// runTurn executes one model round: it checks the context, prepares the
// view, fires the round-start UI callbacks, streams the provider response,
// and records usage. It does NOT process the result — the caller appends
// the assistant message and runs the tool round. Returns the raw provider
// result; on error the round's UI was finished and the session flushed, and
// the caller aborts the turn.
func (a *Agent) runTurn(ctx context.Context, h *llm.StreamHandlers, first bool) (*llm.StreamResult, error) {
	if ctx.Err() != nil {
		finishStreamUI(h)
		a.RepairOrphanToolCalls()
		a.FlushSession()
		return nil, ctx.Err()
	}
	view := a.prepareMessages(ctx, h)

	if first && h.OnStart != nil {
		h.OnStart()
	} else if !first && h.OnRoundStart != nil {
		h.OnRoundStart()
	}
	if ctx.Err() != nil {
		finishStreamUI(h)
		a.RepairOrphanToolCalls()
		a.FlushSession()
		return nil, ctx.Err()
	}

	a.Provider.SetThinkingLevel(string(a.ThinkingLevel))
	result, err := a.Provider.GenerateResponseStream(ctx, view, a.AllowedToolNames(), a.llmTools(), h)
	if err != nil {
		finishStreamUI(h)
		if ctx.Err() != nil {
			a.RepairOrphanToolCalls()
		}
		a.FlushSession()
		return nil, err
	}
	a.recordTurnUsage(result.Usage)
	a.statsMu.Lock()
	a.UsageAccum.Add(result.Usage)
	a.statsMu.Unlock()

	// Attribute the round to the provider-reported model BEFORE the
	// round is finalized: OnStreamEnd (inside finishStreamUI below)
	// writes stream_end, which tells the client the bubble is done. The
	// model must reach the client first so the live bubble can be
	// stamped while it is still current. Fired for every round (not
	// just the final one) so intermediate content+tool rounds get their
	// chip live instead of only via history replay.
	if h.OnReplyModel != nil {
		h.OnReplyModel(result.Model)
	}
	return result, nil
}

// runToolRound executes a round's tool calls: read-only eligible batches run
// concurrently (bounded by maxParallelTools), everything else strictly
// sequentially in model order so mutating tools stay serialized and results
// append in the model's call order. Returns true when the context was
// cancelled mid-execution — the caller must abort the turn (the tool
// result protocol was already preserved for the next turn).
func (a *Agent) runToolRound(ctx context.Context, h *llm.StreamHandlers, toolCalls []llm.ToolCall) bool {
	if a.toolCallsParallelEligible(toolCalls) {
		return a.executeToolCallsParallel(ctx, h, toolCalls)
	}
	for i, tc := range toolCalls {
		if ctx.Err() != nil {
			// Preserve a valid tool-call/result protocol for the next turn.
			a.appendCanceledToolResults(toolCalls[i:])
			finishStreamUI(h)
			a.FlushSession()
			return true
		}
		if h.OnToolCall != nil {
			h.OnToolCall(tc)
		}
		if h.OnToolExecute != nil {
			h.OnToolExecute(tc.Name)
		}

		// Attach the live-output sink (if any) to this tool call's
		// context so shell tools can stream chunks to the UI as the
		// command runs. The sink is per-tool so each command gets its
		// own identity (id/name) in the handler.
		toolCtx := ctx
		if h.OnToolOutput != nil {
			toolCtx = ContextWithToolOutput(ctx, func(command, chunk string) {
				h.OnToolOutput(tc.ID, tc.Name, command, chunk)
			})
		}
		res, errTool := a.executeTool(toolCtx, tc)
		if ctx.Err() != nil {
			if errTool == nil {
				res = "The operation was cancelled by the user."
			} else if errTool == context.Canceled {
				res = "The operation was cancelled by the user."
			} else {
				res = formatToolError(res, errTool)
			}
			if h.OnToolResult != nil {
				h.OnToolResult(tc.ID, tc.Name, res, false)
			}
			a.appendToolResult(tc, res)
			a.appendCanceledToolResults(toolCalls[i+1:])
			finishStreamUI(h)
			a.FlushSession()
			return true
		}
		success := errTool == nil
		if errTool != nil {
			res = formatToolError(res, errTool)
		}

		if h.OnToolResult != nil {
			h.OnToolResult(tc.ID, tc.Name, res, success)
		}

		a.appendToolResult(tc, res)
	}
	return false
}
