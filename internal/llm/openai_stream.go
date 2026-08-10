package llm

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go"
)

// reasoningStopGrace is how long the stream waits for content to resume after
// a reasoning-only finish_reason="stop" before treating the response as
// complete. Two-phase streams (reasoning → content) legitimately continue
// within the grace, so only a provider that sends the stop and then HOLDS the
// connection open without [DONE] is cut short — otherwise the read would
// block for the full idle timeout (streamReadIdleTimeout, default 10m). Set
// GOGEN_REASONING_STOP_GRACE=0/off to disable the bound (wait indefinitely).
func reasoningStopGrace() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GOGEN_REASONING_STOP_GRACE"))
	if raw == "" {
		return 30 * time.Second
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "false") {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// streamDrainLimit bounds how many post-finish chunks the accumulator
// consumes while waiting for the final usage chunk. Usage is captured as
// soon as it arrives (processChunk ingests chunk.Usage before the
// streamDone check), so a provider that sends many no-op chunks between
// finish_reason and the usage chunk still gets its usage recorded. The bound
// only protects against a provider that keeps the SSE stream open forever
// without ever sending usage; once usage arrives the drain stops
// immediately.
const streamDrainLimit = 64

// ensureStreamCallbacks fills nil callback fields in h with no-ops,
// and returns a non-nil h. Safe for callers that always need an
// OnToken or OnThinkingToken handler.
func ensureStreamCallbacks(h *StreamHandlers) *StreamHandlers {
	if h == nil {
		h = &StreamHandlers{}
	}
	if h.OnStreamEnd == nil {
		h.OnStreamEnd = func() {}
	}
	if h.OnRecoverPartialStream == nil {
		h.OnRecoverPartialStream = func() {}
	}
	if h.OnToken == nil {
		h.OnToken = func(string) {}
	}
	if h.OnThinkingToken == nil {
		h.OnThinkingToken = func(string) {}
	}
	return h
}

// streamAccumulator accumulates streaming data from an OpenAI
// chat completion stream.
type streamAccumulator struct {
	fullContent      strings.Builder
	fullRefusal      strings.Builder
	fullReasoning    strings.Builder
	lastFinishReason string
	model            string // model ID reported by the provider (from the first chunk that carries it)
	streamUsage      *Usage
	tcAccums         []tcAccum
	tcIndexMap       map[int]int
	extras           extraFieldAccums
	streamDone       bool
	drainAfterDone   int
	// stopPending is set when a reasoning-only finish_reason="stop" arrives:
	// a two-phase stream may continue with content after it, so streamDone
	// stays false, but the consuming loop bounds the wait for the next chunk
	// (see reasoningStopGrace). Cleared as soon as content/refusal/tool-calls
	// resume.
	stopPending bool
	// graceExpired is set when the reasoning-stop grace timer fires (the
	// provider held the connection open without resuming or sending [DONE]),
	// so the loop can distinguish a grace close from a real stream error.
	graceExpired atomic.Bool
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{
		tcIndexMap: make(map[int]int),
		extras:     newExtraFieldAccums(),
	}
}

func (a *streamAccumulator) processChunk(chunk openai.ChatCompletionChunk, onToken, onThinking func(string), h *StreamHandlers) bool {
	if u := usageFromOpenAI(chunk.Usage); u != nil {
		a.streamUsage = u
	}
	// Router/proxy endpoints (e.g. OpenCode Zen) resolve model aliases
	// server-side and report the actual model on every chunk; keep the
	// first non-empty value.
	if a.model == "" && chunk.Model != "" {
		a.model = chunk.Model
	}
	if a.streamDone {
		if a.streamUsage != nil {
			// The usage chunk is the only post-finish data that matters;
			// nothing left to wait for.
			return true
		}
		a.drainAfterDone++
		return a.drainAfterDone >= streamDrainLimit
	}
	if len(chunk.Choices) == 0 {
		return false
	}

	choice := chunk.Choices[0]
	delta := choice.Delta
	a.extras.addFromDelta(delta, onThinking, &a.fullReasoning)

	if delta.Content != "" {
		a.fullContent.WriteString(delta.Content)
		onToken(delta.Content)
		// Content resuming after a reasoning-only stop means the two-phase
		// stream continued; the reasoning-stop grace is no longer needed.
		a.stopPending = false
	}
	if delta.Refusal != "" {
		a.fullRefusal.WriteString(delta.Refusal)
		a.stopPending = false
	}
	if len(delta.ToolCalls) > 0 {
		a.stopPending = false
		for _, tc := range delta.ToolCalls {
			var idx int
			a.tcAccums, idx = mergeToolCallDelta(tc, a.tcAccums, a.tcIndexMap)
			tacc := &a.tcAccums[idx]
			if tc.Function.Name != "" {
				emitToolCallStart(tacc.Index, tacc, h)
			}
			if tc.Function.Arguments != "" {
				emitToolCallStart(tacc.Index, tacc, h)
				emitToolCallArgsDelta(tacc.Index, tacc, tc.Function.Arguments, h)
			}
		}
	}

	if choice.FinishReason != "" {
		switch choice.FinishReason {
		case "tool_calls":
			a.lastFinishReason = choice.FinishReason
			a.streamDone = true
		case "stop":
			if a.fullContent.Len() > 0 || a.fullRefusal.Len() > 0 || len(a.tcAccums) > 0 {
				a.lastFinishReason = choice.FinishReason
				a.streamDone = true
			} else if a.fullReasoning.Len() > 0 {
				// Reasoning-only stop: a two-phase stream (reasoning →
				// content) may continue with content after this chunk, so
				// streamDone stays false. Record the pending state so the
				// consuming loop can bound the wait — a provider that holds
				// the connection open without [DONE] would otherwise block
				// the read for the full idle timeout.
				a.stopPending = true
			}
		default:
			a.lastFinishReason = choice.FinishReason
			a.streamDone = true
		}
	} else if len(a.tcAccums) > 0 && toolAccumsStreamComplete(a.tcAccums) && deltaIsTerminalToolSignal(delta, true) {
		a.lastFinishReason = "tool_calls"
		return true
	}
	return false
}

func emitToolCallStart(tcIdx int, acc *tcAccum, h *StreamHandlers) {
	if acc.Started || acc.Name == "" || h.OnToolCallStart == nil {
		return
	}
	acc.Started = true
	h.OnToolCallStart(tcIdx, acc.ID, acc.Name)
}

func emitToolCallArgsDelta(tcIdx int, acc *tcAccum, argsDelta string, h *StreamHandlers) {
	if argsDelta == "" || h.OnToolCallArgsDelta == nil {
		return
	}
	h.OnToolCallArgsDelta(tcIdx, acc.ID, acc.Name, argsDelta)
}

func (a *streamAccumulator) buildResult() (*StreamResult, error) {
	var toolCalls []ToolCall
	for _, acc := range a.tcAccums {
		if acc.Name == "" {
			continue
		}
		var args map[string]interface{}
		var argsErr string
		if strings.TrimSpace(acc.ArgsStr) == "" {
			args = map[string]interface{}{}
		} else {
			parsed, parseErr := parseToolCallArgs(acc.ArgsStr)
			if parseErr != nil {
				args = map[string]interface{}{}
				argsErr = parseErr.Error()
			} else {
				args = parsed
			}
		}
		toolCalls = append(toolCalls, ToolCall{
			Index:     acc.Index,
			ID:        acc.ID,
			Name:      acc.Name,
			Args:      args,
			ArgsStr:   acc.ArgsStr,
			ArgsError: argsErr,
		})
	}

	if len(toolCalls) == 0 && (a.fullReasoning.Len() > 0 || a.fullContent.Len() > 0) {
		extractedCalls := extractToolCallsFromText(a.fullReasoning.String() + a.fullContent.String())
		if len(extractedCalls) > 0 {
			toolCalls = extractedCalls
		}
	}

	content := a.fullContent.String()

	if a.lastFinishReason == "" && (content != "" || a.fullRefusal.Len() > 0 || a.fullReasoning.Len() > 0 || len(a.tcAccums) > 0) {
		if len(a.tcAccums) > 0 {
			a.lastFinishReason = "tool_calls"
		} else {
			a.lastFinishReason = "stop"
		}
	}

	if a.lastFinishReason == "" && content == "" && a.fullRefusal.Len() == 0 && a.fullReasoning.Len() == 0 && len(toolCalls) == 0 {
		return nil, fmt.Errorf("stream ended without finish_reason")
	}

	return &StreamResult{
		Content:   content,
		Reasoning: a.fullReasoning.String(),
		Refusal:   a.fullRefusal.String(),
		ToolCalls: toolCalls,
		Usage:     a.streamUsage,
		Model:     a.model,
	}, nil
}
