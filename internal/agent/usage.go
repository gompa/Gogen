package agent

import (
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// UsageAccumulator tracks token usage across the session.
// Fields are guarded by Agent.statsMu: the turn goroutine accumulates via
// Add while web probes read via SnapshotUsageAccum without agentMu/turnMu.
// Do not copy or mutate an accumulator without holding statsMu.
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

// SnapshotUsageAccum returns a copy of the session token-usage totals.
// Thread-safe: the turn goroutine accumulates under statsMu while web probes
// read without agentMu/turnMu.
func (a *Agent) SnapshotUsageAccum() UsageAccumulator {
	a.statsMu.RLock()
	u := a.UsageAccum
	a.statsMu.RUnlock()
	return u
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
