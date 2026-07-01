package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

const (
	contextSourceAPI        = "api"
	contextSourceEstimated  = "estimated"
)

// TurnContext summarizes context window usage for the current conversation.
type TurnContext struct {
	Snapshot       contextmgr.ContextSnapshot
	LastUsage      *llm.Usage
	UsedSource     string
	PromptTokens   int
	CompletionTokens int
}

func (a *Agent) recordTurnUsage(u *llm.Usage) {
	a.lastTurnUsage = u
}

func (a *Agent) ContextStats(ctx context.Context) TurnContext {
	view := a.prepareMessages(ctx)
	var snap contextmgr.ContextSnapshot
	if a.Context != nil {
		snap = a.Context.Snapshot(a.Messages, view)
	} else {
		snap = contextmgr.ContextSnapshot{
			MessageCount: len(a.Messages),
		}
	}

	stats := TurnContext{
		Snapshot:   snap,
		LastUsage:  a.lastTurnUsage,
		UsedSource: contextSourceEstimated,
	}

	if a.lastTurnUsage != nil && a.lastTurnUsage.PromptTokens > 0 {
		stats.UsedSource = contextSourceAPI
		stats.PromptTokens = a.lastTurnUsage.PromptTokens
		stats.CompletionTokens = a.lastTurnUsage.CompletionTokens
		stats.Snapshot.Used = a.lastTurnUsage.PromptTokens
		stats.Snapshot.NearCompact = stats.Snapshot.CompactAt > 0 && stats.Snapshot.Used >= stats.Snapshot.CompactAt
		if stats.Snapshot.Limit > 0 {
			stats.Snapshot.Percent = float64(stats.Snapshot.Used) / float64(stats.Snapshot.Limit)
		}
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
	source := stats.UsedSource
	if source == contextSourceAPI {
		source = "last request"
	}
	line := fmt.Sprintf("context: %s / %s", formatTokenCount(snap.Used), formatTokenCount(snap.Limit))
	if snap.Limit > 0 {
		pct := int(snap.Percent * 100)
		if pct > 100 {
			pct = 100
		}
		line += fmt.Sprintf(" (%d%%)", pct)
	}
	if source != "" {
		line += " · " + source
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

	sourceLabel := "estimated"
	if stats.UsedSource == contextSourceAPI {
		sourceLabel = "last request (api)"
	}
	fmt.Fprintf(&b, "Context (%s)\n", sourceLabel)
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
	if snap.Stored > 0 && snap.Stored != snap.Used {
		fmt.Fprintf(&b, "  Stored:   %s tokens (canonical history)\n", formatTokenCount(snap.Stored))
	}
	if snap.ToolTruncated {
		b.WriteString("  Tool truncation: active for LLM view\n")
	}
	if stats.PromptTokens > 0 || stats.CompletionTokens > 0 {
		fmt.Fprintf(&b, "  Last turn: %s in / %s out\n",
			formatTokenCount(stats.PromptTokens),
			formatTokenCount(stats.CompletionTokens))
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
	if n < 10000 {
		whole := n / 1000
		frac := (n % 1000) / 100
		if frac == 0 {
			return fmt.Sprintf("%dk", whole)
		}
		return fmt.Sprintf("%d.%dk", whole, frac)
	}
	if n < 1000000 {
		whole := n / 1000
		frac := (n % 1000) / 100
		if frac == 0 {
			return fmt.Sprintf("%dk", whole)
		}
		return fmt.Sprintf("%d.%dk", whole, frac)
	}
	return fmt.Sprintf("%d", n)
}
