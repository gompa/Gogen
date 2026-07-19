package contextmgr

import (
	"sort"
	"fmt"
	"hash/fnv"
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

type tokenCacheEntry struct {
	n  int
	fp uint64 // fingerprint of role+content+tool metadata
}

// tokenCache caches token counts by message pointer to avoid re-tokenizing
// the same messages across multiple calls to EstimateTokens within a turn.
// Entries are validated with a content fingerprint so GC address reuse and
// in-place mutations cannot return stale counts.
//
// The fingerprint is a 64-bit FNV-1a hash. A collision (two distinct message
// bodies hashing identically while reusing the same pointer) would let a
// stale count be served silently. With the ~birthday bound at 2^32 messages
// (far beyond any real session) and the cache only gating an *estimate* used
// to decide compaction timing — never correctness of tool output — this trade
// is acceptable. If the cache ever gated a correctness-critical decision,
// switch to a 128-bit hash (crypto/sha128 or hash/maphash with a per-process
// seed) for defense-in-depth.
//
// Memory for entries of compacted/removed messages persists until
// InvalidateTokenCache is called. That is intentional and fine: the map is
// bounded by session size, and fingerprint checks prevent stale reads.
var tokenCache struct {
	sync.RWMutex
	m map[any]tokenCacheEntry
}

// initTokenCache lazily initializes the token cache map.
func initTokenCache() {
	tokenCache.Lock()
	defer tokenCache.Unlock()
	if tokenCache.m == nil {
		tokenCache.m = make(map[any]tokenCacheEntry)
	}
}

// cachedTokenCount returns the cached token count for a message when the
// fingerprint still matches.
func cachedTokenCount(msg *llm.Message) (int, bool) {
	tokenCache.RLock()
	defer tokenCache.RUnlock()
	if tokenCache.m == nil {
		return 0, false
	}
	e, ok := tokenCache.m[msg]
	if !ok || e.fp != messageFingerprint(msg) {
		return 0, false
	}
	return e.n, true
}

// storeTokenCount caches the token count for a message.
func storeTokenCount(msg *llm.Message, n int) {
	initTokenCache()
	tokenCache.Lock()
	tokenCache.m[msg] = tokenCacheEntry{n: n, fp: messageFingerprint(msg)}
	tokenCache.Unlock()
}

// InvalidateTokenCache clears all cached entries. Call this when the message
// list is compacted, restored, or otherwise reassigned. Per-message content
// mutations are caught by the fingerprint check inside cachedTokenCount, so
// appending a new message to the slice does NOT require invalidation: the
// existing entries' message pointers and fingerprints still match.
func InvalidateTokenCache() {
	tokenCache.Lock()
	tokenCache.m = nil
	tokenCache.Unlock()
}

// TokenCacheSize reports the number of cached token-count entries.
// Intended for tests; production callers should not depend on cache size.
func TokenCacheSize() int {
	tokenCache.RLock()
	defer tokenCache.RUnlock()
	if tokenCache.m == nil {
		return 0
	}
	return len(tokenCache.m)
}

func sortedToolArgKeys(args map[string]interface{}) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func messageFingerprint(msg *llm.Message) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(msg.Role))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(msg.Content))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(msg.ToolCallID))
	for _, tc := range msg.ToolCalls {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(tc.ID))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(tc.Name))
		_, _ = h.Write([]byte{0})
		if tc.ArgsStr != "" {
			_, _ = h.Write([]byte(tc.ArgsStr))
		} else {
			for _, k := range sortedToolArgKeys(tc.Args) {
				v := tc.Args[k]
				_, _ = h.Write([]byte(k))
				_, _ = h.Write([]byte{0})
				_, _ = h.Write([]byte(fmt.Sprint(v)))
				_, _ = h.Write([]byte{0})
			}
		}
	}
	return h.Sum64()
}

// EstimateTokens approximates token count for a message list using cl100k_base
// (GPT-family). Falls back to a bytes/4 heuristic if the tokenizer is unavailable.
// Results are cached by message pointer (with fingerprint validation) to avoid
// re-tokenizing within a turn.
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
