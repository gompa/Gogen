package contextmgr

import (
	"fmt"
	"sort"
	"sync"

	"gogen/internal/llm"

	"github.com/tiktoken-go/tokenizer"
)

var (
	encOnce sync.Once
	codec   tokenizer.Codec
	encErr  error
)

func getCodec() (tokenizer.Codec, error) {
	encOnce.Do(func() {
		codec, encErr = tokenizer.Get(tokenizer.Cl100kBase)
	})
	return codec, encErr
}

// WarmTokenizer eagerly loads the cl100k_base tokenizer vocabulary so the
// first token-counting call does not pay the ~2.6 MB init cost inline.
func WarmTokenizer() {
	_, _ = getCodec()
}

// TokenCounts returns per-message token counts for the given messages.
// Token counts are computed fresh each time; the cl100k_base tokenizer
// is fast enough that a global cache added more complexity than value.
func (m *Manager) TokenCounts(messages []llm.Message) []int {
	if len(messages) == 0 {
		return nil
	}
	counts := make([]int, len(messages))
	for i := range messages {
		counts[i] = computeMessageTokens(messages[i])
	}
	return counts
}

// ComputeMessageTokens returns the estimated token count for a single message
// using the cl100k_base tokenizer when available, falling back to a bytes/4 heuristic.
func ComputeMessageTokens(msg llm.Message) int {
	return computeMessageTokens(msg)
}

func sortedToolArgKeys(args map[string]interface{}) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EstimateTokens approximates token count for a message list using cl100k_base
// (GPT-family). Falls back to a bytes/4 heuristic if the tokenizer is unavailable.
// No caching; the tokenizer is fast enough for on-demand use.
func (m *Manager) EstimateTokens(messages []llm.Message) int {
	total := 0
	for i := range messages {
		total += computeMessageTokens(messages[i])
	}
	return total
}

func computeMessageTokens(msg llm.Message) int {
	if n, ok := countTokensExact(msg); ok {
		return n
	}
	return estimateMessageTokensHeuristic(msg)
}

func countTokensExact(msg llm.Message) (int, bool) {
	c, err := getCodec()
	if err != nil || c == nil {
		return 0, false
	}
	tokens := 4 // role/message framing overhead
	ids, _, err := c.Encode(msg.Content)
	if err != nil {
		return 0, false
	}
	tokens += len(ids)
	if msg.Reasoning != "" {
		if ids, _, err := c.Encode(msg.Reasoning); err == nil {
			tokens += len(ids)
		}
	}
	if msg.Refusal != "" {
		if ids, _, err := c.Encode(msg.Refusal); err == nil {
			tokens += len(ids)
		}
	}
	for _, tc := range msg.ToolCalls {
		tokens += 4
		if ids, _, err := c.Encode(tc.Name); err == nil {
			tokens += len(ids)
		}
		if ids, _, err := c.Encode(tc.ID); err == nil {
			tokens += len(ids)
		}
		if tc.ArgsStr != "" {
			if ids, _, err := c.Encode(tc.ArgsStr); err == nil {
				tokens += len(ids)
			}
		} else {
			for _, k := range sortedToolArgKeys(tc.Args) {
				v := tc.Args[k]
				if ids, _, err := c.Encode(k); err == nil {
					tokens += len(ids)
				}
				if ids, _, err := c.Encode(fmt.Sprint(v)); err == nil {
					tokens += len(ids)
				}
				tokens += 2
			}
		}
	}
	if msg.ToolCallID != "" {
		if ids, _, err := c.Encode(msg.ToolCallID); err == nil {
			tokens += len(ids)
		}
	}
	return tokens, true
}

func estimateMessageTokensHeuristic(msg llm.Message) int {
	tokens := (len(msg.Content) + 3) / 4
	tokens += (len(msg.Reasoning) + 3) / 4
	tokens += (len(msg.Refusal) + 3) / 4
	tokens += 4 // role/overhead
	for _, tc := range msg.ToolCalls {
		tokens += (len(tc.Name)+len(tc.ID)+12)/4 + 4
		if tc.ArgsStr != "" {
			tokens += (len(tc.ArgsStr) + 3) / 4
		} else {
			for _, k := range sortedToolArgKeys(tc.Args) {
				v := tc.Args[k]
				tokens += (len(k)+len(fmt.Sprint(v))+4)/4 + 2
			}
		}
	}
	if msg.ToolCallID != "" {
		tokens += (len(msg.ToolCallID) + 4) / 4
	}
	return tokens
}

func truncateForSummary(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	c, err := getCodec()
	if err == nil && c != nil {
		ids, _, err := c.Encode(text)
		if err == nil {
			if len(ids) <= maxTokens {
				return text
			}
			decoded, derr := c.Decode(ids[:maxTokens])
			if derr == nil {
				return decoded + fmt.Sprintf("\n… truncated for summarization (%d tokens total)", len(ids))
			}
		}
	}
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	return text[:maxChars] + fmt.Sprintf("\n… truncated for summarization (%d chars total)", len(text))
}
