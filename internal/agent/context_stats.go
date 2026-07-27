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
	a.lastTurnUsage = u
	// Record baseline for accurate context stats: the API's PromptTokens
	// is the exact token count for the view (system prompt + canonical msgs)
	// at this point. Subsequent ContextStats calls will use this as the
	// authoritative baseline and only estimate messages added afterward.
	if u.PromptTokens > 0 {
		a.apiBaselinePromptTokens = u.PromptTokens
		a.apiBaselineMsgCount = len(a.Messages)
	}
}

// clearTurnUsage clears last-turn API counters and the baseline used by
// ContextStats. Call when messages are compacted, the model is switched, or
// the session is reset — any time the stored usage no longer represents the
// current conversation state.
func (a *Agent) clearTurnUsage() {
	a.lastTurnUsage = nil
	a.apiBaselinePromptTokens = 0
	a.apiBaselineMsgCount = 0
}

// ContextStats is a read-only probe of current context usage.
// It must not mutate Messages, compact history, or call the provider.
// Web callers must hold Server.agentMu (see internal/server/agent_sync.go).
//
// When an API baseline is available, Snapshot.Used reflects the API's exact
// prompt_tokens for the messages that were in the last request, plus local
// estimates for any messages appended since then.
func (a *Agent) ContextStats(ctx context.Context) TurnContext {
	// Abort before expensive tokenization when the caller already cancelled
	// (web/TUI interrupt). Returning a minimal snapshot keeps cancel paths
	// from stalling inside OnStart/OnRoundStart context probes.
	if ctx != nil && ctx.Err() != nil {
		return TurnContext{
			Snapshot: contextmgr.ContextSnapshot{MessageCount: len(a.Messages)},
		}
	}
	msgs := a.Messages
	view := msgs
	if a.Context != nil {
		// Copy so Snapshot iteration is stable if the caller releases agentMu
		// and another turn appends (append may reallocate).
		if n := len(msgs); n > 0 {
			cp := make([]llm.Message, n)
			copy(cp, msgs)
			msgs = cp
		}
		view = withSystemPrompt(msgs, a.WorkingDir)
		// Use cached profile only — do not run DetectProjectProfile here.
		view = enrichSystemPrompt(view, a.WorkingDir, a.ProjectFilePath, a.ProjectGuidelines, a.projectProfile, a.Mode)
	}

	var snap contextmgr.ContextSnapshot
	if a.Context != nil {
		// Use pre-computed token counts from a restored session snapshot to
		// avoid re-tokenizing every message (expensive for large sessions).
		// The counts are valid only when they match the current message count.
		if len(a.restoredTokenCounts) == len(msgs) {
			snap = a.Context.SnapshotWithCounts(msgs, view, a.restoredTokenCounts)
		} else {
			snap = a.Context.Snapshot(msgs, view)
		}
	} else {
		snap = contextmgr.ContextSnapshot{
			MessageCount: len(msgs),
		}
	}

	// If we have an API baseline, use it as the authoritative count for
	// messages that were in the last request, then add local estimates
	// for any messages appended since then.
	if a.lastTurnUsage != nil && a.apiBaselinePromptTokens > 0 && a.apiBaselineMsgCount > 0 && a.Context != nil {
		baseline := a.apiBaselinePromptTokens
		if n := len(msgs); n > a.apiBaselineMsgCount {
			extra := msgs[a.apiBaselineMsgCount:]
			baseline += a.Context.EstimateTokens(extra)
		}
		snap.Used = baseline
		if snap.Limit > 0 {
			snap.Percent = float64(baseline) / float64(snap.Limit)
		}
	}

	stats := TurnContext{
		Snapshot:  snap,
		LastUsage: a.lastTurnUsage,
	}

	// Attach last-request API counters for detail views.
	if a.lastTurnUsage != nil && a.lastTurnUsage.PromptTokens > 0 {
		stats.PromptTokens = a.lastTurnUsage.PromptTokens
		stats.CompletionTokens = a.lastTurnUsage.CompletionTokens
		stats.CachedTokens = a.lastTurnUsage.CachedTokens
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
	if snap.CompactAt > 0 {
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
