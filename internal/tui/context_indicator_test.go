package tui

import (
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
)

// TestContextIndicatorUpdatesDuringStreaming is a regression test: the
// status-bar context indicator previously only refreshed at streamEndMsg /
// streamErrorMsg (refreshContextStats reads a.Messages, which races with
// the streaming goroutine mutating it mid-turn), so it stayed frozen at the
// pre-turn value for the entire duration of a streamed response. It should
// now move as tokens/thinking/tool-args arrive, using a safe local estimate,
// and land on the exact value once refreshContextStats runs at stream end.
func TestContextIndicatorUpdatesDuringStreaming(t *testing.T) {
	m := Model{
		chatLines:           nil,
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
		contextStats: agent.TurnContext{
			Snapshot: contextmgr.ContextSnapshot{
				Used:  1000,
				Limit: 100000,
			},
		},
	}
	m.contextLine = agent.FormatContextBrief(m.contextStats)
	before := m.contextLine

	// Start of a new turn: baseline is captured from the last authoritative
	// measurement above.
	m.handleStreamStart()

	m.handleStreamToken(strings.Repeat("a", 400)) // ~100 estimated tokens
	afterOneToken := m.contextLine

	if afterOneToken == before {
		t.Fatalf("context indicator did not update after a streamed token: still %q", afterOneToken)
	}
	if !strings.Contains(afterOneToken, "(est.)") {
		t.Fatalf("expected live estimate to be marked as approximate, got %q", afterOneToken)
	}

	m.handleStreamToken(strings.Repeat("b", 400)) // more tokens -> estimate should grow further
	afterTwoTokens := m.contextLine
	if afterTwoTokens == afterOneToken {
		t.Fatalf("context indicator did not move on a second streamed token: still %q", afterTwoTokens)
	}

	// Simulate the authoritative refresh done at stream end landing on an
	// exact figure that differs from the rough estimate.
	m.contextStats = agent.TurnContext{
		Snapshot: contextmgr.ContextSnapshot{Used: 1180, Limit: 100000},
	}
	m.contextLine = agent.FormatContextBrief(m.contextStats)
	if strings.Contains(m.contextLine, "(est.)") {
		t.Fatalf("final refreshed indicator should not be marked as an estimate: %q", m.contextLine)
	}
}
