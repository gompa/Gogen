package agent

import (
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// UsageAccumulator tracks token usage across the session.
type UsageAccumulator struct {
	TotalPromptTokens     int
	TotalCompletionTokens int
	TotalCachedTokens     int
	TotalTurns            int
}

// Add accumulates a usage snapshot.
func (u *UsageAccumulator) Add(usage *llm.Usage) {
	if usage == nil {
		return
	}
	u.TotalPromptTokens += usage.PromptTokens
	u.TotalCompletionTokens += usage.CompletionTokens
	u.TotalCachedTokens += usage.CachedTokens
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		u.TotalTurns++
	}
}

// Format returns a human-readable summary of accumulated usage.
func (u *UsageAccumulator) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session usage: %d turns", u.TotalTurns)
	if u.TotalPromptTokens > 0 {
		fmt.Fprintf(&b, ", %s in / %s out",
			formatTokenCount(u.TotalPromptTokens),
			formatTokenCount(u.TotalCompletionTokens))
	}
	if u.TotalCachedTokens > 0 {
		fmt.Fprintf(&b, ", %s cached", formatTokenCount(u.TotalCachedTokens))
	}
	return b.String()
}
