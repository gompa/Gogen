package agent

import (
	"context"
	"errors"

	"gogen/internal/llm"
)

// toolRoundOutcome reports how the agent loop must continue after a tool
// round.
type toolRoundOutcome int

const (
	// toolRoundContinue: results were appended; the loop may call the model
	// again.
	toolRoundContinue toolRoundOutcome = iota
	// toolRoundCancelled: the context was cancelled mid-round; the caller
	// aborts with ctx.Err().
	toolRoundCancelled
	// toolRoundStopped: a patch_file failure ended the turn (marker-only
	// diff or exhausted per-turn mismatch budget); the caller returns
	// stopMsg to the host instead of calling the model again.
	toolRoundStopped
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
	view, err := a.prepareMessages(ctx, h)
	if err != nil {
		// The pre-flight check could not make the request fit and the
		// last-resort mode is "error" (Phase 0e): the diagnostic is
		// returned instead of letting the provider refuse the request.
		finishStreamUI(h)
		a.RepairOrphanToolCalls()
		a.FlushSession()
		return nil, err
	}

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
// append in the model's call order. Returns the round outcome and, for
// toolRoundStopped, the final tool-result message to surface as the turn
// outcome. On toolRoundCancelled the tool result protocol was already
// preserved for the next turn.
func (a *Agent) runToolRound(ctx context.Context, h *llm.StreamHandlers, toolCalls []llm.ToolCall) (toolRoundOutcome, string) {
	if a.toolCallsParallelEligible(toolCalls) {
		// Mutating tools (patch_file included) are never parallel-eligible,
		// so the patch-turn stop cannot occur on this path.
		if a.executeToolCallsParallel(ctx, h, toolCalls) {
			// The cancelled round's assistant message and tool results were
			// appended in memory; persist them now. Without this flush the
			// session is left CLEAN with unsaved state, and the shutdown /
			// eviction sweep (FlushPending writes only dirty sessions) and
			// the TUI's /resume (flushes only when dirty) would silently
			// lose the round. Mirrors the sequential cancel path above.
			a.FlushSession()
			return toolRoundCancelled, ""
		}
		return toolRoundContinue, ""
	}
	for i, tc := range toolCalls {
		if ctx.Err() != nil {
			// Preserve a valid tool-call/result protocol for the next turn.
			a.appendCanceledToolResults(toolCalls[i:])
			finishStreamUI(h)
			a.FlushSession()
			return toolRoundCancelled, ""
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
		// own identity (id/name) in the handler. read_image gets a
		// per-tool image sink the same way: the handler only collects
		// the image into the sink, and the synthetic user message is
		// appended right after the tool result below, so the
		// tool-call/result protocol is never violated.
		toolCtx := ctx
		imgSink := &imageSink{}
		if h.OnToolOutput != nil {
			toolCtx = ContextWithToolOutput(toolCtx, func(command, chunk string) {
				h.OnToolOutput(tc.ID, tc.Name, command, chunk)
			})
		}
		if h.OnToolOutputEnd != nil {
			toolCtx = ContextWithToolOutputEnd(toolCtx, func(success bool) {
				h.OnToolOutputEnd(tc.ID, success)
			})
		}
		toolCtx = ContextWithImageSink(toolCtx, imgSink)
		res, errTool := a.executeTool(toolCtx, tc)
		if errors.Is(errTool, errPatchTurnStop) {
			// The model is stuck in a patch retry loop: stop the turn. This
			// call's result is appended normally; any remaining calls in the
			// round get cancelled placeholders so the tool-call/result
			// protocol stays valid, and the caller returns stopMsg to the
			// host instead of letting the model write another attempt.
			res = formatToolError(res, errTool)
			if h.OnToolResult != nil {
				h.OnToolResult(tc.ID, tc.Name, res, false)
			}
			a.appendToolResult(tc, res)
			a.appendImageMessages(imgSink)
			a.appendCanceledToolResults(toolCalls[i+1:])
			finishStreamUI(h)
			a.FlushSession()
			return toolRoundStopped, res
		}
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
			a.appendImageMessages(imgSink)
			a.appendCanceledToolResults(toolCalls[i+1:])
			finishStreamUI(h)
			a.FlushSession()
			return toolRoundCancelled, ""
		}
		success := errTool == nil
		if errTool != nil {
			res = formatToolError(res, errTool)
		}

		if h.OnToolResult != nil {
			h.OnToolResult(tc.ID, tc.Name, res, success)
		}

		a.appendToolResult(tc, res)
		a.appendImageMessages(imgSink)
	}
	return toolRoundContinue, ""
}
