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
		acc.ArgsStr += tc.Function.Arguments
	}
	return tcAccums, idx
}

func parseToolCallArgs(argsStr string) (map[string]interface{}, error) {
	s := strings.TrimSpace(argsStr)
	if s == "" {
		return map[string]interface{}{}, nil
	}
	var args map[string]interface{}
	err := json.Unmarshal([]byte(s), &args)
	if err != nil {
		return nil, err
	}
	if args == nil {
		return map[string]interface{}{}, nil
	}
	return args, nil
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
	for _, acc := range accums {
		if acc.Name == "" || !toolArgsFullyReceived(acc.ArgsStr) {
			return false
		}
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
