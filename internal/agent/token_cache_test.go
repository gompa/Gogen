package agent

import (
	"context"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestStreamProcessInputPreservesTokenCache is a regression test for a bug
// where StreamProcessInput called `defer contextmgr.InvalidateTokenCache()`
// at the end of every turn, wiping the entire token cache even though:
//   - historical messages were not mutated (their pointers + content
//     fingerprints still match the cache keys), and
//   - the new user message added in this turn was either already cached or
//     gets cached on first access, so the cache cannot be "stale".
//
// Wiping the cache forces the next turn to re-tokenize the entire
// conversation from scratch — defeating the entire purpose of the
// pointer-keyed cache. See internal/contextmgr/tokens.go for the cache
// contract.
//
// We populate the cache directly here (rather than relying on
// ShouldCompact -> EstimateTokens) so the assertion is independent of
// KeepRecentMessages settings and exercises the exact contract the bug
// violated.
func TestStreamProcessInputPreservesTokenCache(t *testing.T) {
	contextmgr.InvalidateTokenCache()

	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	// Pre-populate the cache with a small history. Pre-allocating capacity
	// ensures the subsequent StreamProcessInput append (to a.Messages) does
	// not reallocate the backing array, so the cached message addresses
	// remain valid.
	a.Messages = make([]llm.Message, 0, 8)
	a.Messages = append(a.Messages,
		llm.Message{Role: "user", Content: "first message"},
		llm.Message{Role: "assistant", Content: "first reply"},
	)
	if n := ctxMgr.EstimateTokens(a.Messages); n <= 0 {
		t.Fatalf("pre-populate: expected non-zero token count, got %d", n)
	}
	cachedBefore := contextmgr.TokenCacheSize()
	if cachedBefore == 0 {
		t.Fatal("pre-populate: expected cache to be populated")
	}

	if _, err := a.StreamProcessInput(context.Background(), "second message", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}

	// After the turn the cache must still hold the entries populated above.
	// prepareMessages may have added entries for the newly appended
	// messages, so the size can grow but must not drop to zero.
	cachedAfter := contextmgr.TokenCacheSize()
	if cachedAfter < cachedBefore {
		t.Fatalf("token cache shrank across a turn: before=%d after=%d "+
			"(the between-turn invalidation bug is back)",
			cachedBefore, cachedAfter)
	}
}

// TestStreamProcessInputCacheSurvivesAcrossTurns makes the regression
// intent explicit: a second turn should reuse cache entries populated by
// the first turn, not re-tokenize the whole history.
func TestStreamProcessInputCacheSurvivesAcrossTurns(t *testing.T) {
	contextmgr.InvalidateTokenCache()

	provider := &statsStubProvider{limit: 1000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	a.Messages = make([]llm.Message, 0, 8)
	a.Messages = append(a.Messages,
		llm.Message{Role: "user", Content: "first message"},
		llm.Message{Role: "assistant", Content: "first reply"},
	)
	if n := ctxMgr.EstimateTokens(a.Messages); n <= 0 {
		t.Fatalf("pre-populate: expected non-zero token count, got %d", n)
	}

	if _, err := a.StreamProcessInput(context.Background(), "second message", nil); err != nil {
		t.Fatalf("first StreamProcessInput: %v", err)
	}
	afterFirst := contextmgr.TokenCacheSize()
	if afterFirst == 0 {
		t.Fatal("token cache was wiped after first turn")
	}

	if _, err := a.StreamProcessInput(context.Background(), "third message", nil); err != nil {
		t.Fatalf("second StreamProcessInput: %v", err)
	}
	afterSecond := contextmgr.TokenCacheSize()
	// The second turn should at minimum preserve every entry from the
	// first (historical message pointers are unchanged) and add at most
	// one new entry for the newly appended user/assistant messages.
	if afterSecond < afterFirst {
		t.Fatalf("token cache shrank across turns: afterFirst=%d afterSecond=%d",
			afterFirst, afterSecond)
	}
}
