package llm

import "github.com/openai/openai-go"

func usageFromOpenAI(u openai.CompletionUsage) *Usage {
	cached := int(u.PromptTokensDetails.CachedTokens)
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 && cached == 0 {
		return nil
	}
	return &Usage{
		PromptTokens:     int(u.PromptTokens),
		CompletionTokens: int(u.CompletionTokens),
		TotalTokens:      int(u.TotalTokens),
		CachedTokens:     cached,
	}
}
