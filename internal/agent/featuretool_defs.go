package agent

import (
	"sync"

	"gogen/internal/llm"
)

// Precomputed feature-tool definitions.
//
// The *ToolDef constructors below are pure functions of constant inputs, but
// featureTools runs many times per round — llmTools once, allowsTool once per
// candidate name in plan mode, AllowedToolNames, and featureToolFor once per
// tool call — and it used to rebuild every JSON schema on each call
// (BenchmarkAgentFeatureTools: ~12µs and 186 allocations per round). This
// mirrors builtinToolDefs, whose schemas are likewise constructed once at
// package init.
//
// The definitions are SHARED, so nothing may mutate them — the same rule
// llmTools already follows for builtinToolDefs (it copies the slice, never the
// schemas). TestFeatureToolDefsArePrecomputed fails if a frozen definition
// drifts from what its constructor returns today, which is the only real risk
// of freezing them.
var (
	boardTool = boardToolDef()

	subagentForkTool   = subagentForkToolDef()
	listAgentsTool     = listAgentsToolDef()
	getAgentResultTool = getAgentResultToolDef()
	waitAgentTool      = waitAgentToolDef()
	sendMessageTool    = sendMessageToolDef()
	interruptAgentTool = interruptAgentToolDef()
	skillTool          = skillsToolDef()
	terminalTool       = terminalToolDef()
	bashPersistentTool = bashPersistentToolDef()
	searchMeaningTool  = searchMeaningToolDef()

	// mcpGatewayTools is the nested-mode MCP set. The constructors state that
	// every definition here is CONSTANT (no tool counts, server names, or
	// health), which is exactly what makes freezing it safe.
	mcpGatewayTools = mcpGatewayFeatureTools()
)

// subagentToolDefsCache memoizes subagentToolDef. Unlike the other feature
// schemas it depends on two live inputs — whether the installed spawner is
// continuable, and the configured subagent_max_concurrent, which the
// description quotes — so the cache key carries both. Identical inputs must
// produce the identical definition: the serialized tool list is the provider
// prompt-cache prefix.
//
// sync.Map because featureTools runs on the turn goroutine while hosts also
// read it from probes (a web config echo). The key space is (bool, small
// int), so the memo cannot grow without bound.
var subagentToolDefsCache sync.Map // subagentToolDefKey -> llm.Tool

type subagentToolDefKey struct {
	background    bool
	maxConcurrent int
}

// precomputedSubagentToolDef returns the memoized subagent schema for the
// given spawner capability and concurrency limit.
func precomputedSubagentToolDef(bg bool, maxConcurrent int) llm.Tool {
	key := subagentToolDefKey{background: bg, maxConcurrent: maxConcurrent}
	if cached, ok := subagentToolDefsCache.Load(key); ok {
		return cached.(llm.Tool)
	}
	cached, _ := subagentToolDefsCache.LoadOrStore(key, subagentToolDef(bg, maxConcurrent))
	return cached.(llm.Tool)
}
