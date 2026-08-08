package llm

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go"
)

type tcAccum struct {
	Index   int
	ID      string
	Name    string
	ArgsStr string
	Started bool
	// argsFinalized is cached once ArgsStr is verified to be a complete JSON
	// object via toolArgsFullyReceived. Reset to false whenever ArgsStr grows.
	// The cheap brace-depth pre-check avoids re-validating an incomplete
	// buffer on every streamed argument fragment.
	argsFinalized bool
}

func toolCallDeltaArgsOnly(tc openai.ChatCompletionChunkChoiceDeltaToolCall) bool {
	return tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments != ""
}

// mergeToolCallDelta folds one streamed tool-call fragment into the accumulators.
// Local OpenAI-compatible servers often omit index on argument continuations (defaulting
// to 0), which would otherwise splice tool N's JSON onto tool 0.
func mergeToolCallDelta(
	tc openai.ChatCompletionChunkChoiceDeltaToolCall,
	tcAccums []tcAccum,
	tcIndexMap map[int]int,
) ([]tcAccum, int) {
	tcIdx := int(tc.Index)

	if toolCallDeltaArgsOnly(tc) {
		if len(tcAccums) > 0 {
			lastIdx := len(tcAccums) - 1
			if last := tcAccums[lastIdx]; last.Index > tcIdx {
				return applyToolCallDelta(tcAccums, lastIdx, tc)
			}
		}
		if mapIdx, ok := tcIndexMap[tcIdx]; ok {
			return applyToolCallDelta(tcAccums, mapIdx, tc)
		}
		if len(tcAccums) > 0 {
			lastIdx := len(tcAccums) - 1
			return applyToolCallDelta(tcAccums, lastIdx, tc)
		}
	}

	if mapIdx, ok := tcIndexMap[tcIdx]; ok {
		// Some servers reuse index 0 for every sequential tool call. A new
		// non-empty ID means a distinct call — do not append onto the prior one.
		if tc.ID != "" && tcAccums[mapIdx].ID != "" && tc.ID != tcAccums[mapIdx].ID {
			tcIndexMap[tcIdx] = len(tcAccums)
			tcAccums = append(tcAccums, tcAccum{
				Index:   tcIdx,
				ID:      tc.ID,
				Name:    tc.Function.Name,
				ArgsStr: tc.Function.Arguments,
			})
			return tcAccums, len(tcAccums) - 1
		}
		return applyToolCallDelta(tcAccums, mapIdx, tc)
	}

	tcIndexMap[tcIdx] = len(tcAccums)
	tcAccums = append(tcAccums, tcAccum{
		Index:   tcIdx,
		ID:      tc.ID,
		Name:    tc.Function.Name,
		ArgsStr: tc.Function.Arguments,
	})
	return tcAccums, len(tcAccums) - 1
}

func applyToolCallDelta(tcAccums []tcAccum, idx int, tc openai.ChatCompletionChunkChoiceDeltaToolCall) ([]tcAccum, int) {
	acc := &tcAccums[idx]
	if tc.ID != "" {
		acc.ID = tc.ID
	}
	if tc.Function.Name != "" {
		acc.Name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		merged := mergeToolArgsDelta(acc.ArgsStr, tc.Function.Arguments)
		if merged != acc.ArgsStr {
			// Buffer grew/replaced: invalidate the finalized cache so it is
			// re-evaluated lazily by maybeFinalizeArgs.
			acc.argsFinalized = false
			acc.ArgsStr = merged
		}
		acc.maybeFinalizeArgs()
	}
	return tcAccums, idx
}

// maybeFinalizeArgs runs the expensive json.Unmarshal validity check exactly
// once per accumulator and only when the (trimmed) buffer structurally looks
// like a complete single JSON object. While braces are still open — the
// common streaming case — the cheap O(n) brace-depth scan returns early and
// no JSON validation runs. This preserves toolArgsFullyReceived semantics
// while avoiding O(n^2) re-validation of an incomplete growing buffer.
func (a *tcAccum) maybeFinalizeArgs() {
	if a.argsFinalized {
		return
	}
	s := strings.TrimSpace(a.ArgsStr)
	if s == "" || s[0] != '{' || s[len(s)-1] != '}' {
		return
	}
	if completeJSONObject(s) {
		a.argsFinalized = toolArgsFullyReceived(a.ArgsStr)
	}
}

// completeJSONObject reports whether s is a single JSON object spanning the
// whole buffer: it starts with '{' and the string-aware scanner (which skips
// quoted strings, honoring escapes) finds the matching '}' exactly at the
// end. Unlike a naive brace-depth count, braces inside strings do not confuse
// it. Used by maybeFinalizeArgs as the cheap pre-check before the
// authoritative json.Unmarshal validation.
func completeJSONObject(s string) bool {
	_, end := extractJSONObject(s, 0)
	return end == len(s)
}

// mergeToolArgsDelta combines streamed argument fragments.
// Providers may send true deltas, cumulative snapshots, or full-object replays;
// naive concatenation of the latter two yields "invalid character '{' ..." errors.
func mergeToolArgsDelta(existing, delta string) string {
	if delta == "" {
		return existing
	}
	if existing == "" {
		return delta
	}
	// Cumulative snapshot: each chunk re-sends args from the start.
	if strings.HasPrefix(delta, existing) {
		return delta
	}
	// Exact replay of the same fragment/object.
	if delta == existing {
		return existing
	}
	// Already-complete JSON followed by another object start — ignore the replay
	// (common when servers re-emit the finished arguments blob).
	if toolArgsFullyReceived(existing) {
		trimmed := strings.TrimSpace(delta)
		if trimmed == strings.TrimSpace(existing) || strings.HasPrefix(trimmed, "{") {
			return existing
		}
	}
	return existing + delta
}

func parseToolCallArgs(argsStr string) (map[string]interface{}, error) {
	s := strings.TrimSpace(argsStr)
	if s == "" {
		return map[string]interface{}{}, nil
	}
	var args map[string]interface{}
	err := json.Unmarshal([]byte(s), &args)
	if err == nil {
		if args == nil {
			return map[string]interface{}{}, nil
		}
		return args, nil
	}
	// Recovery for duplicated complete objects: {"a":1}{"a":1}
	dec := json.NewDecoder(strings.NewReader(s))
	if decErr := dec.Decode(&args); decErr == nil && args != nil {
		return args, nil
	}
	return nil, err
}

// toolArgsFullyReceived reports whether streamed tool arguments form complete JSON.
func toolArgsFullyReceived(argsStr string) bool {
	s := strings.TrimSpace(argsStr)
	// Empty means args have not started yet (name-only delta) — not complete.
	if s == "" {
		return false
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var m map[string]interface{}
	return json.Unmarshal([]byte(s), &m) == nil
}

// toolAccumsStreamComplete reports whether every accumulated tool call has a name
// and fully received arguments (used to detect llama.cpp tool streams that end
// without finish_reason or [DONE]).
func toolAccumsStreamComplete(accums []tcAccum) bool {
	if len(accums) == 0 {
		return false
	}
	for i := range accums {
		acc := &accums[i]
		if acc.Name == "" {
			return false
		}
		// Use the cached finalized flag from streaming; fall back to a fresh
		// check for accumulators built outside the streaming merge (tests).
		if acc.argsFinalized {
			continue
		}
		if !toolArgsFullyReceived(acc.ArgsStr) {
			return false
		}
		acc.argsFinalized = true
	}
	return true
}

// deltaIsTerminalToolSignal reports llama.cpp's empty-delta chunk that ends a
// tool-call stream when finish_reason is omitted but the connection stays open.
func deltaIsTerminalToolSignal(delta openai.ChatCompletionChunkChoiceDelta, haveTools bool) bool {
	if !haveTools {
		return false
	}
	return deltaIsEmptyDelta(delta)
}

func deltaIsEmptyDelta(delta openai.ChatCompletionChunkChoiceDelta) bool {
	if delta.Content != "" || delta.Refusal != "" || len(delta.ToolCalls) > 0 {
		return false
	}
	for _, field := range delta.JSON.ExtraFields {
		if !field.Valid() {
			continue
		}
		raw := strings.TrimSpace(field.Raw())
		if raw != "" && raw != "null" && raw != `""` {
			return false
		}
	}
	raw := strings.TrimSpace(delta.RawJSON())
	return raw == "{}" || raw == ""
}
