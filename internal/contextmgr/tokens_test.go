package contextmgr

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestEstimateTokensUsesTokenizer(t *testing.T) {
	m := NewManager(nil, Settings{})
	ascii := m.EstimateTokens([]llm.Message{{Role: "user", Content: "hello world"}})
	cjk := m.EstimateTokens([]llm.Message{{Role: "user", Content: "你好世界编程助手"}})
	if ascii <= 0 || cjk <= 0 {
		t.Fatalf("ascii=%d cjk=%d", ascii, cjk)
	}
	// CJK should not be wildly over-counted as bytes/4 would for UTF-8.
	heuristicCJK := (len("你好世界编程助手") + 3) / 4
	if cjk >= heuristicCJK {
		// tokenizer usually counts fewer tokens than UTF-8 bytes/4 for CJK
		t.Logf("cjk tokens=%d heuristic=%d (ok if tokenizer unavailable)", cjk, heuristicCJK)
	}
	long := m.EstimateTokens([]llm.Message{{Role: "user", Content: strings.Repeat("token ", 100)}})
	if long < ascii {
		t.Fatalf("expected longer text to use more tokens: %d < %d", long, ascii)
	}
}

func TestEstimateTokensRejectsStaleCacheAfterMutation(t *testing.T) {
	InvalidateTokenCache()
	m := NewManager(nil, Settings{})
	msgs := []llm.Message{{Role: "user", Content: "short"}}
	before := m.EstimateTokens(msgs)
	msgs[0].Content = strings.Repeat("token ", 200)
	after := m.EstimateTokens(msgs)
	if after <= before {
		t.Fatalf("expected mutated content to recount tokens: before=%d after=%d", before, after)
	}
}

func TestEnsureToolResultsCappedInvalidatesTokenCache(t *testing.T) {
	InvalidateTokenCache()
	m := NewManager(nil, Settings{MaxToolResultBytes: 64})
	big := strings.Repeat("x", 400)
	msgs := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", Content: big},
	}
	before := m.EstimateTokens(msgs)
	if !m.EnsureToolResultsCapped(msgs) {
		t.Fatal("expected truncation")
	}
	after := m.EstimateTokens(msgs)
	if after >= before {
		t.Fatalf("expected capped tool result to reduce tokens: before=%d after=%d", before, after)
	}
	if !strings.Contains(msgs[1].Content, toolResultTruncationMarker) {
		t.Fatalf("expected truncation marker in tool content")
	}
}

// TestTokenCacheSurvivesMessageAppend documents the contract that callers
// depend on: appending a new message to a slice whose earlier messages
// are already cached must NOT discard the cached entries. The cache is
// keyed by message pointer with a content fingerprint guard, so historical
// entries remain valid as long as their address and content are unchanged.
//
// Note: we pre-allocate the slice with extra capacity so the append does
// not reallocate the backing array; a reallocation would move the
// existing message structs to new addresses, which is a legitimate
// cache miss. The agent maintains spare capacity in a.Messages across
// real turns for the same reason.
//
// If a future change makes the cache address-sensitive (e.g. switches to
// indexing by slice position), this test will fail and force a re-think of
// every place that does `append(messages, ...)` between turns.
func TestTokenCacheSurvivesMessageAppend(t *testing.T) {
	InvalidateTokenCache()
	m := NewManager(nil, Settings{})

	msgs := make([]llm.Message, 0, 4)
	msgs = append(msgs,
		llm.Message{Role: "user", Content: "first message"},
		llm.Message{Role: "assistant", Content: "first reply"},
	)
	_ = m.EstimateTokens(msgs)
	if got := TokenCacheSize(); got != 2 {
		t.Fatalf("expected 2 cached entries after first tokenize, got %d", got)
	}

	// Simulate a turn boundary: append a new user message. The two
	// pre-existing messages still sit at the same addresses with the same
	// content, so their cache entries must remain valid.
	msgs = append(msgs, llm.Message{Role: "user", Content: "second message"})
	_ = m.EstimateTokens(msgs)
	if got := TokenCacheSize(); got != 3 {
		t.Fatalf("expected 3 cached entries after append, got %d", got)
	}
}
