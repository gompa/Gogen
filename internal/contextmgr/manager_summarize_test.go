package contextmgr

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

type countingProvider struct {
	stubProvider
	calls int
}

func (c *countingProvider) GenerateResponse(_ context.Context, msgs []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	c.calls++
	return llm.Response{Content: "summary-" + strings.Repeat("x", len(msgs[0].Content)/100)}, nil
}

func TestSummarizeMessagesChunksLargeHistory(t *testing.T) {
	provider := &countingProvider{stubProvider: stubProvider{summary: "ok"}}
	m := NewManager(provider, Settings{
		ContextLimit:         400,
		CompactReserveTokens: 50,
		MaxToolResultBytes:   8192,
	})

	var middle []llm.Message
	for i := 0; i < 8; i++ {
		middle = append(middle, llm.Message{
			Role:    "assistant",
			Content: strings.Repeat("word ", 500),
		})
	}

	out, err := m.summarizeMessagesDepth(context.Background(), middle, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected summary output")
	}
	if provider.calls < 2 {
		t.Fatalf("expected recursive chunking to call provider multiple times, got %d", provider.calls)
	}
}

// TestSummarizeMessagesDepthGuardsRecursion verifies the depth cap in
// summarizeMessagesDepth actually engages: at depth >= maxSummarizeDepth the
// summarizer must stop recursing into the provider and instead return a
// truncated render of the messages. This is a regression guard for a prior
// design where summarizeMessages recursed to itself (bypassing the depth
// guard), making maxSummarizeDepth and summarizeMessagesDepth dead code.
func TestSummarizeMessagesDepthGuardsRecursion(t *testing.T) {
	provider := &countingProvider{stubProvider: stubProvider{summary: "ok"}}
	m := NewManager(provider, Settings{
		ContextLimit:         400,
		CompactReserveTokens: 50,
		MaxToolResultBytes:   8192,
	})
	middle := []llm.Message{{Role: "user", Content: strings.Repeat("word ", 50)}}

	callsBefore := provider.calls
	out, err := m.summarizeMessagesDepth(context.Background(), middle, maxSummarizeDepth)
	if err != nil {
		t.Fatal(err)
	}
	// At the depth cap the guard runs and skips the provider entirely.
	if provider.calls != callsBefore {
		t.Fatalf("expected no provider calls at depth cap, got %d new calls", provider.calls-callsBefore)
	}
	if out == "" {
		t.Fatal("expected truncated render output at depth cap")
	}
	if !strings.Contains(out, "USER:") {
		t.Fatalf("expected render output to contain USER:, got %q", out)
	}
}
