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
	})
	if got == nil {
		t.Fatal("expected usage")
	}
	if got.PromptTokens != 1200 || got.CompletionTokens != 45 || got.TotalTokens != 1245 {
		t.Fatalf("unexpected usage: %+v", got)
	}
}
