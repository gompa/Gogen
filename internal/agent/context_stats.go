package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TurnContext summarizes context window usage for the current conversation.
type TurnContext struct {
	Snapshot         contextmgr.ContextSnapshot
	LastUsage        *llm.Usage
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// recordTurnUsage stores the API usage and records a baseline so ContextStats
// can use the API's exact prompt_tokens for the context indicator.
func (a *Agent) recordTurnUsage(u *llm.Usage) {
	if u == nil {
		return
	}
	a.statsMu.Lock()
	a.lastTurnUsage = u
	// Record baseline for accurate context stats: the API's PromptTokens
	// is the exact token count for the view (system prompt + canonical msgs)
	// at this point. Subsequent ContextStats calls will use this as the
	// authoritative baseline and only estimate messages added afterward.
	if u.PromptTokens > 0 {
		a.apiBaselinePromptTokens = u.PromptTokens
		a.apiBaselineMsgCount = len(a.Messages)
	}
	a.statsMu.Unlock()
}

// clearTurnUsage clears last-turn API counters and the baseline used by
// ContextStats. Call when messages are compacted, the model is switched, or
// the session is reset — any time the stored usage no longer represents the
// current conversation state.
func (a *Agent) clearTurnUsage() {
	a.statsMu.Lock()
	a.lastTurnUsage = nil
	a.apiBaselinePromptTokens = 0
	a.apiBaselineMsgCount = 0
	a.statsMu.Unlock()
}

// ContextStats is a read-only probe of current context usage.
// It must not mutate Messages, compact history, or call the provider.
// Safe to call concurrently with a running turn: the shared state it reads
// (Messages, cached token counts, API usage baseline, project profile) is
// snapshotted under Agent.statsMu before tokenization, which happens without
// holding any server lock. Tokenization is slow, so avoid holding
// Server.agentMu/turnMu across the call where practical; the /context WS path
// still runs it under turnMu, which is safe (see internal/server/agent_sync.go).
//
// When an API baseline is available, Snapshot.Used reflects the API's exact
// prompt_tokens for the messages that were in the last request, plus local
// estimates for any messages appended since then.
func (a *Agent) ContextStats(ctx context.Context) TurnContext {
	// Abort before expensive tokenization when the caller already cancelled
	// (web/TUI interrupt). Returning a minimal snapshot keeps cancel paths
	// from stalling inside OnStart/OnRoundStart context probes.
	if ctx != nil && ctx.Err() != nil {
		a.statsMu.RLock()
		n := len(a.Messages)
		a.statsMu.RUnlock()
		return TurnContext{
			Snapshot: contextmgr.ContextSnapshot{MessageCount: n},
		}
	}

	// Snapshot the shared state (message list, cached token counts, API usage
	// baseline, project profile) under the lock, then release before
	// tokenizing. A concurrent turn goroutine may append messages, extend the
	// cached counts, or record new API usage while ContextStats runs (web
	// readers do not hold agentMu/turnMu). The message clone is deep so
	// tokenization cannot race in-place stabilization on the live array.
	a.statsMu.RLock()
	msgs := cloneMessages(a.Messages)
	counts := append([]int(nil), a.tokenCounts...)
	countsEpoch := a.countsEpoch
	lastUsage := a.lastTurnUsage
	baselinePromptTokens, baselineMsgCount := a.apiBaselinePromptTokens, a.apiBaselineMsgCount
	projectProfile := a.projectProfile
	a.statsMu.RUnlock()

	view := msgs
	if a.Context != nil {
		view = withSystemPrompt(msgs, a.WorkingDir)
		// Use cached profile only — do not run DetectProjectProfile here.
		view = enrichSystemPrompt(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, projectProfile, a.Mode)
	}

	var snap contextmgr.ContextSnapshot
	if a.Context != nil {
		switch {
		case counts == nil:
			// No cached counts (fresh session, or a compaction/restore just
			// cleared them): compute them once for this snapshot, then publish
			// the result so subsequent probes skip re-tokenization entirely.
			// The computed counts stay valid for the cloned snapshot even if
			// the live list changes before the store; the epoch guard keeps
			// stale counts from ever being published.
			counts = make([]int, len(msgs))
			for i := range msgs {
				counts[i] = contextmgr.ComputeMessageTokens(msgs[i])
			}
			snap = a.Context.SnapshotWithCounts(msgs, view, counts)
			a.statsMu.Lock()
			if a.countsEpoch == countsEpoch && len(a.tokenCounts) < len(msgs) {
				a.tokenCounts = append(a.tokenCounts, counts[len(a.tokenCounts):]...)
			}
			a.statsMu.Unlock()
		case len(counts) < len(msgs):
			// The cache is a valid prefix but messages were appended since it
			// was last extended (ContextStats can race a burst of appends).
			// Compute counts for only the missing suffix locally and publish
			// the extension under the epoch guard.
			extended := make([]int, len(msgs))
			copy(extended, counts)
			for i := len(counts); i < len(msgs); i++ {
				extended[i] = contextmgr.ComputeMessageTokens(msgs[i])
			}
			counts = extended
			snap = a.Context.SnapshotWithCounts(msgs, view, counts)
			a.statsMu.Lock()
			if a.countsEpoch == countsEpoch && len(a.tokenCounts) < len(msgs) {
				a.tokenCounts = append(a.tokenCounts, counts[len(a.tokenCounts):]...)
			}
			a.statsMu.Unlock()
		default:
			// Complete cache: the fast path — no tokenization at all.
			snap = a.Context.SnapshotWithCounts(msgs, view, counts)
		}
	} else {
		snap = contextmgr.ContextSnapshot{
			MessageCount: len(msgs),
		}
	}

	// If we have an API baseline, use it as the authoritative count for
	// messages that were in the last request, then add local estimates
	// for any messages appended since then.
	if lastUsage != nil && baselinePromptTokens > 0 && baselineMsgCount > 0 && a.Context != nil {
		baseline := baselinePromptTokens
		if n := len(msgs); n > baselineMsgCount {
			extra := msgs[baselineMsgCount:]
			baseline += a.Context.EstimateTokens(extra)
		}
		snap.Used = baseline
		if snap.Limit > 0 {
			snap.Percent = float64(baseline) / float64(snap.Limit)
		}
	}

	stats := TurnContext{
		Snapshot:  snap,
		LastUsage: lastUsage,
	}

	// Attach last-request API counters for detail views.
	if lastUsage != nil && lastUsage.PromptTokens > 0 {
		stats.PromptTokens = lastUsage.PromptTokens
		stats.CompletionTokens = lastUsage.CompletionTokens
		stats.CachedTokens = lastUsage.CachedTokens
	}

	return stats
}

// HandleContextCommand processes /context.
func (a *Agent) HandleContextCommand(ctx context.Context, input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed != "/context" && trimmed != "context" {
		return "", false
	}
	return FormatContextDetail(a.ContextStats(ctx)), true
}

func FormatContextBrief(stats TurnContext) string {
	snap := stats.Snapshot
	if snap.Limit <= 0 && snap.Used <= 0 {
		return ""
	}
	line := fmt.Sprintf("context: %s / %s", formatTokenCount(snap.Used), formatTokenCount(snap.Limit))
	if snap.Limit > 0 {
		pct := int(snap.Percent * 100)
		if pct > 100 {
			pct = 100
		}
		line += fmt.Sprintf(" (%d%%)", pct)
	}
	if stats.CachedTokens > 0 {
		line += fmt.Sprintf(" · %s cached", formatTokenCount(stats.CachedTokens))
	}
	return line
}

// AppendContextBrief adds an estimated context usage line when stats are available.
func AppendContextBrief(ctx context.Context, a *Agent, message string) string {
	if line := FormatContextBrief(a.ContextStats(ctx)); line != "" {
		return message + "\n" + line
	}
	return message
}

func FormatContextDetail(stats TurnContext) string {
	snap := stats.Snapshot
	var b strings.Builder

	fmt.Fprintf(&b, "Context (estimated)\n")
	fmt.Fprintf(&b, "  Used:     %s / %s", formatTokenCount(snap.Used), formatTokenCount(snap.Limit))
	if snap.Limit > 0 {
		pct := int(snap.Percent * 100)
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(&b, "  (%d%%)", pct)
	}
	b.WriteString("\n")
	if snap.CompactDisabled {
		fmt.Fprintf(&b, "  Compact:  off\n")
	} else if snap.CompactAt > 0 {
		fmt.Fprintf(&b, "  Compact:  auto at %s\n", formatTokenCount(snap.CompactAt))
	}
	fmt.Fprintf(&b, "  Messages: %d\n", snap.MessageCount)
	if snap.ToolTruncated {
		b.WriteString("  Tool truncation: some results capped\n")
	}
	if stats.PromptTokens > 0 || stats.CompletionTokens > 0 {
		fmt.Fprintf(&b, "  Last turn: %s in / %s out",
			formatTokenCount(stats.PromptTokens),
			formatTokenCount(stats.CompletionTokens))
		if stats.CachedTokens > 0 {
			fmt.Fprintf(&b, " (%s cached)", formatTokenCount(stats.CachedTokens))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTokenCount(n int) string {
	if n <= 0 {
		return "—"
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	whole := n / 1000
	frac := (n % 1000) / 100
	if frac == 0 {
		return fmt.Sprintf("%dk", whole)
	}
	return fmt.Sprintf("%d.%dk", whole, frac)
}
