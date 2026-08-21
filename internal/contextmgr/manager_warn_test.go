package contextmgr

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// TestCompactPrimaryPathFitsAtDefaultTrigger verifies that with the default
// trigger settings (threshold 0.85, reserve 4000) a conversation sitting at
// the trigger point still takes the single-request continuation-summary path
// (view prefix + head + middle + instruction) instead of falling back to the
// legacy flattened-text summarizer. This is the guarantee that lets
// auto-compaction fire late: the summary request itself stays within
// limit - reserve. If a future change raises the threshold or the reserve so
// the request no longer fits at the trigger, this test fails loudly.
func TestCompactPrimaryPathFitsAtDefaultTrigger(t *testing.T) {
	provider := &recordingProvider{}
	m := NewManager(provider, Settings{
		ContextLimit:              20000,
		CompactThreshold:          config.DefaultCompactThreshold,
		CompactReserveTokens:      config.DefaultCompactReserveTokens,
		CompactKeepRecentMessages: config.DefaultCompactKeepRecentMessages,
	})
	viewPrefix := []llm.Message{{Role: "system", Content: "You are a coding agent."}}

	// Grow the conversation until the pre-compaction estimate reaches the
	// trigger budget (compactAt) — the point a real turn would compact at.
	msgs := []llm.Message{{Role: "user", Content: "first task"}}
	compactAt := m.CompactBudget()
	for i := 0; m.EstimateTokens(msgs) < compactAt && len(msgs) < 1000; i++ {
		msgs = append(msgs,
			llm.Message{Role: "user", Content: fmt.Sprintf("question %d: how should we route requests across regions?", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("answer %d: sharded key-value store with consistent hashing and quorum reads.", i)},
		)
	}
	// Messages beyond the keep window so the middle to summarize is non-empty.
	for i := 0; i < config.DefaultCompactKeepRecentMessages+2; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: "tail message"})
	}

	out, _, err := m.CompactPinned(context.Background(), viewPrefix, msgs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected the single-request summary path at the default trigger, got %d requests (fell back to flattened-text summarizer?)", len(provider.requests))
	}
	req := provider.requests[0]
	if req[0].Content != "You are a coding agent." {
		t.Fatalf("expected view prefix first in summary request, got %q", req[0].Content)
	}
	if last := req[len(req)-1]; last.Role != "user" || !strings.Contains(last.Content, "Summarize everything after the first user message") {
		t.Fatalf("expected user-role summary instruction last, got role=%q content=%q", last.Role, last.Content)
	}
	if budget := m.summaryRequestBudgetLocked(); m.EstimateTokens(req) > budget {
		t.Fatalf("summary request (%d tokens) exceeds continuation budget %d", m.EstimateTokens(req), budget)
	}
	if len(out) == 0 || out[0].Content != "first task" {
		t.Fatalf("head not preserved after compaction: %+v", out)
	}
}

// TestCompactBudgetAtDefaultSettingsSmallModels pins the trigger budget floor
// for small context windows: at the default threshold (0.85) and reserve
// (4000) an 8k model triggers at 0.85*8000-4000 = 2800 tokens. This guards
// against raising CompactReserveTokens along with the threshold — 0.85*8000
// -8000 < 0 would floor the trigger to 1000 tokens and re-compact on every
// turn for small local models.
func TestCompactBudgetAtDefaultSettingsSmallModels(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:         8000,
		CompactThreshold:     config.DefaultCompactThreshold,
		CompactReserveTokens: config.DefaultCompactReserveTokens,
	})
	if got := m.CompactBudget(); got < 2000 {
		t.Fatalf("compact budget for an 8k window at default settings = %d, want >= 2000", got)
	}
}

// TestSnapshotWarnNearDecoupledFromTrigger verifies the near-compact warning
// fires before the auto-compaction trigger (75% of the window vs. the trigger
// budget), so the web banner offers lead time, and stays off entirely when
// auto-compaction is disabled.
func TestSnapshotWarnNearDecoupledFromTrigger(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:         100000,
		CompactThreshold:     0.85,
		CompactReserveTokens: 4000,
	})
	// compactAt = 81000; warnAt = min(75000, 81000) = 75000.
	cases := []struct {
		name string
		used int
		near bool
		warn bool
	}{
		{"below warning point", 70000, false, false},
		{"between warning and trigger", 76000, false, true},
		{"at trigger", 82000, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := m.snapshot(nil, nil, tc.used)
			if snap.NearCompact != tc.near || snap.WarnNearCompact != tc.warn {
				t.Fatalf("used=%d: NearCompact=%v WarnNearCompact=%v, want %v/%v",
					tc.used, snap.NearCompact, snap.WarnNearCompact, tc.near, tc.warn)
			}
		})
	}

	// Auto-compaction disabled: no warning regardless of usage.
	off := NewManager(&stubProvider{}, Settings{ContextLimit: 100000, CompactThreshold: 0})
	snap := off.snapshot(nil, nil, 90000)
	if snap.NearCompact || snap.WarnNearCompact {
		t.Fatalf("disabled: NearCompact=%v WarnNearCompact=%v, want both false", snap.NearCompact, snap.WarnNearCompact)
	}
}
