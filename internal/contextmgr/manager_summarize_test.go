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
