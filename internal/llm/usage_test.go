package llm

import (
	"testing"

	"github.com/openai/openai-go"
)

func TestUsageFromOpenAI(t *testing.T) {
	if usageFromOpenAI(openai.CompletionUsage{}) != nil {
		t.Fatal("expected nil for zero usage")
	}
	got := usageFromOpenAI(openai.CompletionUsage{
		PromptTokens:     1200,
		CompletionTokens: 45,
		TotalTokens:      1245,
		PromptTokensDetails: openai.CompletionUsagePromptTokensDetails{
			CachedTokens: 800,
		},
	})
	if got == nil {
		t.Fatal("expected usage")
	}
	if got.PromptTokens != 1200 || got.CompletionTokens != 45 || got.TotalTokens != 1245 {
		t.Fatalf("unexpected usage: %+v", got)
	}
	if got.CachedTokens != 800 {
		t.Fatalf("cached tokens = %d, want 800", got.CachedTokens)
	}
}
