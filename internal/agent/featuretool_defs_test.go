package agent

import (
	"encoding/json"
	"testing"

	"gogen/internal/llm"
)

// mustJSON renders a tool definition for byte comparison: json.Marshal sorts
// map keys, so two structurally identical schemas always marshal identically.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestFeatureToolDefsArePrecomputed guards the frozen schemas in
// featuretool_defs.go against drift: each var must stay byte-identical to what
// its constructor produces. Editing a constructor without refreshing its frozen
// value would silently ship the old schema to the model — the one real risk of
// precomputing, and the reason this test exists.
func TestFeatureToolDefsArePrecomputed(t *testing.T) {
	tests := []struct {
		name   string
		frozen llm.Tool
		fresh  llm.Tool
	}{
		{name: "board", frozen: boardTool, fresh: boardToolDef()},
		{name: "subagent_fork", frozen: subagentForkTool, fresh: subagentForkToolDef()},
		{name: "list_agents", frozen: listAgentsTool, fresh: listAgentsToolDef()},
		{name: "get_agent_result", frozen: getAgentResultTool, fresh: getAgentResultToolDef()},
		{name: "wait_agent", frozen: waitAgentTool, fresh: waitAgentToolDef()},
		{name: "send_message", frozen: sendMessageTool, fresh: sendMessageToolDef()},
		{name: "interrupt_agent", frozen: interruptAgentTool, fresh: interruptAgentToolDef()},
		{name: "skill", frozen: skillTool, fresh: skillsToolDef()},
		{name: "terminal", frozen: terminalTool, fresh: terminalToolDef()},
		{name: "bash_persistent", frozen: bashPersistentTool, fresh: bashPersistentToolDef()},
		{name: "search_meaning", frozen: searchMeaningTool, fresh: searchMeaningToolDef()},
		{name: "subagent continuable", frozen: precomputedSubagentToolDef(true, 4), fresh: subagentToolDef(true, 4)},
		{name: "subagent foreground", frozen: precomputedSubagentToolDef(false, 4), fresh: subagentToolDef(false, 4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := mustJSON(t, tt.frozen), mustJSON(t, tt.fresh); got != want {
				t.Fatalf("frozen definition drifted from its constructor:\n got: %s\nwant: %s", got, want)
			}
		})
	}

	t.Run("mcp gateway", func(t *testing.T) {
		fresh := mcpGatewayFeatureTools()
		if len(fresh) != len(mcpGatewayTools) {
			t.Fatalf("gateway tools = %d, want %d", len(mcpGatewayTools), len(fresh))
		}
		for i, want := range fresh {
			got := mcpGatewayTools[i]
			if got.Name != want.Name {
				t.Fatalf("gateway tool %d = %q, want %q (order is part of the prompt prefix)", i, got.Name, want.Name)
			}
			if a, b := mustJSON(t, got.Definition), mustJSON(t, want.Definition); a != b {
				t.Fatalf("frozen %s definition drifted:\n got: %s\nwant: %s", got.Name, a, b)
			}
			if got.PlanAllowed != want.PlanAllowed {
				t.Fatalf("%s PlanAllowed = %v, want %v", got.Name, got.PlanAllowed, want.PlanAllowed)
			}
		}
	})
}

// TestPrecomputedSubagentToolDefIsDeterministic pins the memo contract: the
// same inputs must yield the same bytes (a provider prompt cache is keyed on
// the serialized tool list), and a different concurrency limit must not be
// served from another key's entry.
func TestPrecomputedSubagentToolDefIsDeterministic(t *testing.T) {
	first := mustJSON(t, precomputedSubagentToolDef(true, 7))
	second := mustJSON(t, precomputedSubagentToolDef(true, 7))
	if first != second {
		t.Fatalf("memoized definition is not byte-stable:\n%s\n%s", first, second)
	}
	if other := mustJSON(t, precomputedSubagentToolDef(true, 8)); other == first {
		t.Fatal("a different max_concurrent must produce a different description")
	}
	if other := mustJSON(t, precomputedSubagentToolDef(false, 7)); other == first {
		t.Fatal("the foreground schema must differ from the continuable one")
	}
}
