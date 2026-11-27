package agent

import (
	"testing"

	"gogen/internal/config"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/skills"
)

// BenchmarkFeatureToolDefs measures the cost of building every feature tool
// definition from scratch (the JSON-schema construction). It is the "before"
// number: featureTools no longer pays this per call — it uses the frozen
// definitions in featuretool_defs.go — which is what BenchmarkAgentFeatureTools
// measures. Kept so the raw construction cost stays visible when a schema
// grows.
func BenchmarkFeatureToolDefs(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		defs := []llm.Tool{
			boardToolDef(),
			subagentToolDef(true, 4),
			subagentForkToolDef(),
			listAgentsToolDef(),
			getAgentResultToolDef(),
			waitAgentToolDef(),
			sendMessageToolDef(),
			interruptAgentToolDef(),
			skillsToolDef(),
			terminalToolDef(),
			bashPersistentToolDef(),
		}
		if len(defs) == 0 {
			b.Fatal("no defs")
		}
	}
}

// BenchmarkFeatureToolsPolicy measures the gating function itself (feature
// on/off checks, manager lookups, closure creation) with no feature wired.
func BenchmarkFeatureToolsPolicy(b *testing.B) {
	a := &Agent{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := a.featureTools(); len(got) != 0 {
			b.Fatalf("unexpected tools: %d", len(got))
		}
	}
}

// BenchmarkAgentFeatureTools measures a.featureTools() on an agent with EVERY
// feature wired — the real per-round path: llmTools (view.go), allowsTool per
// candidate name, AllowedToolNames, and featureToolFor per tool call all call
// it, so its cost is paid many times per round. Compare with
// BenchmarkFeatureToolDefs, which measures the raw schema constructors.
func BenchmarkAgentFeatureTools(b *testing.B) {
	a := newAllFeaturesBenchAgent(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := a.featureTools(); len(got) == 0 {
			b.Fatal("no feature tools")
		}
	}
}

// newAllFeaturesBenchAgent wires every feature-tool source so featureTools
// returns the whole set (board, the continuable subagent cascade, skill, the
// terminal pair, search_meaning, and the nested MCP gateway).
func newAllFeaturesBenchAgent(b *testing.B) *Agent {
	b.Helper()
	dir := b.TempDir()
	a := NewAgent(llmtest.NewMockProvider(), NewExecutor(dir), nil)
	a.SetBoardEnabled(true)
	a.SetBoardManager(NewBoardManager(dir, false))
	a.SetSubagentsEnabled(true)
	a.SetSubagentSpawner(&fakeContinuableSpawner{fakeSpawner: fakeSpawner{report: "r"}})
	a.SetSkillsEnabled(true)
	a.SetSkillsManager(skills.NewManager(dir, false))
	a.SetTerminalEnabled(true)
	a.SetCodeIndexEnabled(true)
	a.SetCodeIndexConfig(func() config.Config { return config.Config{CodeIndexProvider: "test"} })
	a.SetMCPTooling(true, true)
	return a
}
