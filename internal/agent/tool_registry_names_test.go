package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"gogen/internal/llm"
)

// countingMCPRegistry is an MCPToolRegistry that records how often the
// whole-map ToolNames() is called, so a test can prove the hot paths use the
// membership test (HasTool) instead.
type countingMCPRegistry struct {
	names          map[string]struct{}
	toolNamesCalls atomic.Int32
}

func (c *countingMCPRegistry) Definitions() []llm.Tool { return nil }

func (c *countingMCPRegistry) ToolNames() map[string]struct{} {
	c.toolNamesCalls.Add(1)
	out := make(map[string]struct{}, len(c.names))
	for name := range c.names {
		out[name] = struct{}{}
	}
	return out
}

func (c *countingMCPRegistry) HasTool(name string) bool {
	_, ok := c.names[name]
	return ok
}

func (c *countingMCPRegistry) CallTool(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

// TestToolNamesNotCopiedOnHotPaths pins the fix for the surviving
// ToolNames() caller: MCPToolRegistry.HasTool was added so the
// parallel-eligibility scan and the plan-mode allowlist would stop copying
// every registered tool name on each round. AllowedToolNames still has to
// ENUMERATE the registry names (they are part of its result), so it is
// expected to call ToolNames exactly once — the invariant there is that the
// cost does not multiply per tool call.
func TestToolNamesNotCopiedOnHotPaths(t *testing.T) {
	t.Run("parallel eligibility uses membership tests only", func(t *testing.T) {
		reg := &countingMCPRegistry{names: map[string]struct{}{"mcp_search": {}, "mcp_fetch": {}}}
		a := &Agent{}
		a.SetMCPRegistry(reg)
		calls := []llm.ToolCall{
			{ID: "c1", Name: "read_file"},
			{ID: "c2", Name: "search_code"},
			{ID: "c3", Name: "read_files"},
		}
		if !a.toolCallsParallelEligible(calls) {
			t.Fatal("read-only batch should stay parallel-eligible")
		}
		if got := reg.toolNamesCalls.Load(); got != 0 {
			t.Fatalf("ToolNames() calls = %d during the parallel-eligibility scan, want 0", got)
		}
		// An MCP-shadowed name must still force the sequential path — via
		// HasTool, without copying the map.
		reg.names["read_file"] = struct{}{}
		if a.toolCallsParallelEligible(calls) {
			t.Fatal("an MCP-shadowed name must force the sequential path")
		}
		if got := reg.toolNamesCalls.Load(); got != 0 {
			t.Fatalf("ToolNames() calls = %d after a shadowed batch, want 0", got)
		}
	})

	t.Run("plan-mode allowlist checks use membership tests only", func(t *testing.T) {
		reg := &countingMCPRegistry{names: map[string]struct{}{"mcp_search": {}}}
		a := &Agent{Mode: ModePlan}
		a.SetMCPRegistry(reg)
		if a.allowsTool("mcp_search") {
			t.Fatal("inlined MCP tools stay out of plan mode")
		}
		if _, ok := a.featureToolFor("mcp_search"); ok {
			t.Fatal("an MCP-shadowed feature tool name must not resolve")
		}
		if got := reg.toolNamesCalls.Load(); got != 0 {
			t.Fatalf("ToolNames() calls = %d during per-tool plan-mode checks, want 0", got)
		}
	})

	t.Run("AllowedToolNames enumerates the registry exactly once", func(t *testing.T) {
		reg := &countingMCPRegistry{names: map[string]struct{}{"mcp_search": {}}}
		a := &Agent{}
		a.SetMCPRegistry(reg)
		names := a.AllowedToolNames()
		if _, ok := names["mcp_search"]; !ok {
			t.Fatal("AllowedToolNames must include the inlined registry tool")
		}
		if got := reg.toolNamesCalls.Load(); got != 1 {
			t.Fatalf("ToolNames() calls = %d for one AllowedToolNames, want 1", got)
		}
	})
}
