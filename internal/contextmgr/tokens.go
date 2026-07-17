package contextmgr

import (
	"fmt"
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

// tokenCache caches token counts by message pointer to avoid re-tokenizing
// the same messages across multiple calls to EstimateTokens within a turn.
var tokenCache struct {
	sync.RWMutex
	m map[any]int
}

// initTokenCache lazily initializes the token cache map.
func initTokenCache() {
	tokenCache.Lock()
	defer tokenCache.Unlock()
	if tokenCache.m == nil {
		tokenCache.m = make(map[any]int)
	}
}

// cachedTokenCount returns the cached token count for a message, or 0 if
// the message is not cached.
func cachedTokenCount(msg *llm.Message) (int, bool) {
	tokenCache.RLock()
	defer tokenCache.RUnlock()
	if tokenCache.m == nil {
		return 0, false
	}
	n, ok := tokenCache.m[msg]
	return n, ok
}

// storeTokenCount caches the token count for a message.
func storeTokenCount(msg *llm.Message, n int) {
	initTokenCache()
	tokenCache.Lock()
	tokenCache.m[msg] = n
	tokenCache.Unlock()
}

// invalidateTokenCache clears all cached entries. Call this when the message
// list is compacted, messages are mutated, or between turns to bound memory.
func invalidateTokenCache() {
	tokenCache.Lock()
	tokenCache.m = nil
	tokenCache.Unlock()
}

// EstimateTokens approximates token count for a message list using cl100k_base
// (GPT-family). Falls back to a bytes/4 heuristic if the tokenizer is unavailable.
// Results are cached by message pointer to avoid re-tokenizing within a turn.
func (m *Manager) EstimateTokens(messages []llm.Message) int {
	total := 0
	for i := range messages {
		if n, ok := cachedTokenCount(&messages[i]); ok {
			total += n
		} else {
			n := computeMessageTokens(messages[i])
			storeTokenCount(&messages[i], n)
			total += n
		}
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
			for k, v := range tc.Args {
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
	tokens += 4 // role/overhead
	for _, tc := range msg.ToolCalls {
		tokens += (len(tc.Name)+len(tc.ID)+12)/4 + 4
		if tc.ArgsStr != "" {
			tokens += (len(tc.ArgsStr) + 3) / 4
		} else {
			for k, v := range tc.Args {
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
