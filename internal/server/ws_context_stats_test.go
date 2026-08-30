package server

import (
	"encoding/json"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
)

// TestApplyContextStatsWireFormat pins the context-stats wire contract the
// web badge relies on: usedTokens is ALWAYS emitted (0 for a fresh session)
// so clients can distinguish "0 used" from "no context data at all", while
// contextLimit stays omitempty (0 = unknown window size).
func TestApplyContextStatsWireFormat(t *testing.T) {
	tests := []struct {
		name      string
		snap      contextmgr.ContextSnapshot
		wantLimit int
		wantUsed  int
		wantJSON  []string // substrings that must appear in the marshaled message
		notInJSON []string // substrings that must not appear
	}{
		{
			name:      "fresh session: limit known, used zero",
			snap:      contextmgr.ContextSnapshot{Limit: 200000, LimitResolved: true, MessageCount: 0},
			wantLimit: 200000,
			wantUsed:  0,
			wantJSON:  []string{`"contextLimit":200000`, `"usedTokens":0`},
		},
		{
			name:      "mid-session: both set",
			snap:      contextmgr.ContextSnapshot{Limit: 200000, LimitResolved: true, Used: 1234, MessageCount: 3, Percent: 0.006},
			wantLimit: 200000,
			wantUsed:  1234,
			wantJSON:  []string{`"contextLimit":200000`, `"usedTokens":1234`},
		},
		{
			name:      "no context manager: limit unknown, used zero",
			snap:      contextmgr.ContextSnapshot{MessageCount: 0},
			wantLimit: 0,
			wantUsed:  0,
			wantJSON:  []string{`"usedTokens":0`},
			notInJSON: []string{`"contextLimit"`},
		},
		{
			name: "unresolved limit: display fallback must not masquerade as the model window",
			snap: contextmgr.ContextSnapshot{Limit: 128000, MessageCount: 0},
			// Limit is the 128k fallback but LimitResolved is false: the
			// wire must stay silent (absent = unknown) so the client can
			// seed the badge from the model list instead.
			wantLimit: 0,
			wantUsed:  0,
			wantJSON:  []string{`"usedTokens":0`},
			notInJSON: []string{`"contextLimit"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg WSMessage
			applyContextStats(&msg, agent.TurnContext{Snapshot: tt.snap}, nil)
			if msg.ContextLimit != tt.wantLimit {
				t.Fatalf("ContextLimit = %d, want %d", msg.ContextLimit, tt.wantLimit)
			}
			if msg.UsedTokens != tt.wantUsed {
				t.Fatalf("UsedTokens = %d, want %d", msg.UsedTokens, tt.wantUsed)
			}
			b, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, sub := range tt.wantJSON {
				if !strings.Contains(string(b), sub) {
					t.Errorf("marshaled message missing %s: %s", sub, b)
				}
			}
			for _, sub := range tt.notInJSON {
				if strings.Contains(string(b), sub) {
					t.Errorf("marshaled message should not contain %s: %s", sub, b)
				}
			}
		})
	}
}
