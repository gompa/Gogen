package llm

import (
	"encoding/json"
	"strings"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go"
)

type extraFieldAccums map[string]*strings.Builder

func newExtraFieldAccums() extraFieldAccums {
	return make(extraFieldAccums)
}

var streamDisplayExtraFields = map[string]bool{
	"reasoning_content": true,
	"reasoning":         true,
	"thinking":          true,
	"thought":           true,
	"analysis":          true,
}

func extraFieldShouldDisplay(key string) bool {
	if streamDisplayExtraFields[key] {
		return true
	}
	lower := strings.ToLower(key)
	return strings.Contains(lower, "reason") ||
		strings.Contains(lower, "think") ||
		strings.Contains(lower, "thought")
}

func (a extraFieldAccums) addFromDelta(delta openai.ChatCompletionChunkChoiceDelta, onThinking func(string)) {
	extraCount := 0
	for key, field := range delta.JSON.ExtraFields {
		if !field.Valid() {
			continue
		}
		extraCount++
		a.ingestPiece(key, field.Raw(), onThinking)
	}
	// llama.cpp exposes reasoning via ExtraFields; re-parsing RawJSON every
	// chunk was doubling work and stalling the stream loop on large sessions.
	if extraCount == 0 {
		ingestRawDeltaObject(delta.RawJSON(), a, onThinking, nil)
	}
}

func ingestRawDeltaObject(raw string, a extraFieldAccums, onThinking func(string), skipKeys map[string]struct{}) {
	if raw == "" {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return
	}
	for key, val := range obj {
		if key == "role" || key == "tool_calls" || key == "content" || key == "refusal" {
			continue
		}
		if skipKeys != nil {
			if _, skip := skipKeys[key]; skip {
				continue
			}
		}
		a.ingestPiece(key, string(val), onThinking)
	}
}

func (a extraFieldAccums) ingestPiece(key, raw string, onThinking func(string)) {
	if raw == "" || raw == "null" {
		return
	}
	piece := decodeJSONFieldText(raw)
	if piece == "" {
		return
	}
	if a[key] == nil {
		a[key] = &strings.Builder{}
	}
	a[key].WriteString(piece)
	if onThinking != nil && extraFieldShouldDisplay(key) {
		onThinking(piece)
	}
}

func decodeJSONFieldText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		for _, nestedKey := range []string{"text", "content", "value", "data", "reasoning", "thinking"} {
			if v, ok := obj[nestedKey]; ok {
				if text := decodeJSONFieldText(string(v)); text != "" {
					return text
				}
			}
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		var parts []string
		for _, item := range arr {
			if text := decodeJSONFieldText(string(item)); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	}
	return raw
}

func (a extraFieldAccums) primaryDisplayText() string {
	for _, key := range []string{"reasoning_content", "reasoning", "thinking", "thought", "analysis"} {
		if b := a[key]; b != nil {
			if s := strings.TrimSpace(b.String()); s != "" {
				return s
			}
		}
	}
	for key, b := range a {
		if extraFieldShouldDisplay(key) && b != nil {
			if s := strings.TrimSpace(b.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

func (a extraFieldAccums) textLen() int {
	total := 0
	for _, b := range a {
		total += b.Len()
	}
	return total
}

func (a extraFieldAccums) snapshot() map[string]string {
	if len(a) == 0 {
		return nil
	}
	out := make(map[string]string, len(a))
	for k, b := range a {
		if s := strings.TrimSpace(b.String()); s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func primaryDisplayFromExtrasMap(extras map[string]string) string {
	if len(extras) == 0 {
		return ""
	}
	acc := newExtraFieldAccums()
	for k, v := range extras {
		if acc[k] == nil {
			acc[k] = &strings.Builder{}
		}
		acc[k].WriteString(v)
	}
	return acc.primaryDisplayText()
}

func extraFieldsFromMessage(msg openai.ChatCompletionMessage) map[string]string {
	out := make(map[string]string)
	for key, field := range msg.JSON.ExtraFields {
		if !field.Valid() {
			continue
		}
		if text := decodeJSONFieldText(field.Raw()); text != "" {
			out[key] = text
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func logNonStreamResponse(model, source string, content, refusal, displayContent string, extras map[string]string, toolCalls []ToolCall, usage *Usage) {
	tools := make([]debuglog.LLMToolCallRecord, 0, len(toolCalls))
	for _, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Args)
		tools = append(tools, debuglog.LLMToolCallRecord{
			Index:    tc.Index,
			ID:       tc.ID,
			Name:     tc.Name,
			Args:     tc.Args,
			ArgsJSON: string(argsJSON),
		})
	}
	var usageMap map[string]int
	if usage != nil {
		usageMap = map[string]int{
			"promptTokens":     usage.PromptTokens,
			"completionTokens": usage.CompletionTokens,
			"totalTokens":      usage.TotalTokens,
		}
	}
	debuglog.WriteLLMResponse(debuglog.LLMResponseRecord{
		Model:          model,
		Source:         source,
		Content:        content,
		Refusal:        refusal,
		DisplayContent: displayContent,
		Reasoning:      primaryDisplayFromExtrasMap(extras),
		ExtraFields:    extras,
		ToolCalls:      tools,
		Usage:          usageMap,
	})
}
