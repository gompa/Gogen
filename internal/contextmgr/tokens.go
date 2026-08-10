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
	count := messageCounterFor()
	counts := make([]int, len(messages))
	for i := range messages {
		counts[i] = computeMessageTokens(messages[i], count) + imageTokenEstimate(messages[i])
	}
	return counts
}

// ComputeMessageTokens returns the estimated token count for a single message
// using the cl100k_base tokenizer when available, falling back to a bytes/4 heuristic.
func ComputeMessageTokens(msg llm.Message) int {
	return computeMessageTokens(msg, messageCounterFor()) + imageTokenEstimate(msg)
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
// No caching; the tokenizer is fast enough for on-demand use. Sums the
// per-message counts directly (no []int allocation, unlike TokenCounts).
func (m *Manager) EstimateTokens(messages []llm.Message) int {
	if len(messages) == 0 {
		return 0
	}
	count := messageCounterFor()
	total := 0
	for i := range messages {
		total += computeMessageTokens(messages[i], count) + imageTokenEstimate(messages[i])
	}
	return total
}

// imageTokenEstimate is a conservative flat per-image token estimate.
// OpenAI bills vision input at 85 base tokens + 170 per 512x512 tile (high
// detail); without decoding dimensions from the data URL, 1024 tokens per
// image comfortably covers typical screenshots. Overestimating only makes
// compaction run slightly earlier — underestimating would risk overflowing
// the context window.
func imageTokenEstimate(msg llm.Message) int {
	return 1024 * len(msg.Images)
}

// messageCounter counts the tokens in a single string. Both counting
// strategies (tokenizer-backed exact and bytes/4 heuristic) implement this so
// computeMessageTokens can walk the message structure exactly once.
type messageCounter func(string) int

// messageCounterFor resolves the counting strategy once per call: the
// cl100k_base tokenizer when available, else the bytes/4 heuristic.
func messageCounterFor() messageCounter {
	if count, ok := exactMessageCounter(); ok {
		return count
	}
	return heuristicCountString
}

// exactMessageCounter returns a tokenizer-backed messageCounter, or ok=false
// when the tokenizer is unavailable. An encode failure for a single string
// counts that string as 0 tokens rather than failing the whole message.
func exactMessageCounter() (messageCounter, bool) {
	c, err := getCodec()
	if err != nil || c == nil {
		return nil, false
	}
	return func(s string) int {
		ids, _, err := c.Encode(s)
		if err != nil {
			return 0
		}
		return len(ids)
	}, true
}

// heuristicCountString approximates tokens as bytes/4 for a single string.
func heuristicCountString(s string) int {
	return (len(s) + 3) / 4
}

// computeMessageTokens walks the message structure once and counts every
// string through count: content, reasoning, refusal, each tool call's
// name/id/arguments, and the tool-call id. Both the exact and heuristic
// strategies share this single walk, so what gets counted can never drift
// between them.
func computeMessageTokens(msg llm.Message, count messageCounter) int {
	tokens := 4 // role/message framing overhead
	tokens += count(msg.Content)
	tokens += count(msg.Reasoning)
	tokens += count(msg.Refusal)
	for _, tc := range msg.ToolCalls {
		tokens += 4
		tokens += count(tc.Name)
		tokens += count(tc.ID)
		if tc.ArgsStr != "" {
			tokens += count(tc.ArgsStr)
		} else {
			for _, k := range sortedToolArgKeys(tc.Args) {
				v := tc.Args[k]
				tokens += count(k)
				tokens += count(fmt.Sprint(v))
				tokens += 2
			}
		}
	}
	if msg.ToolCallID != "" {
		tokens += count(msg.ToolCallID)
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
