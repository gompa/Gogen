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

func (a *Agent) recordTurnUsage(u *llm.Usage) {
	if u == nil {
		return
	}
	a.lastTurnUsage = u
}

// ContextStats is a read-only probe of current context usage.
// It must not mutate Messages, compact history, or call the provider.
// Web callers must hold Server.agentMu (see internal/server/agent_sync.go).
func (a *Agent) ContextStats(ctx context.Context) TurnContext {
	_ = ctx // reserved for future cancellation of pure-local work
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
		snap = a.Context.Snapshot(msgs, view)
	} else {
		snap = contextmgr.ContextSnapshot{
			MessageCount: len(msgs),
		}
	}

	stats := TurnContext{
		Snapshot:  snap,
		LastUsage: a.lastTurnUsage,
	}

	// Attach last-request API counters for detail views. Snapshot.Used stays
	// as the estimated current history size so the indicator moves as the
	// session grows (API prompt tokens are frozen at request start).
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
