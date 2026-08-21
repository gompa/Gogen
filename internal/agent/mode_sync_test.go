package agent

import (
	"strings"
	"testing"
)

// TestBuiltinToolNamesMatchSchemasAndHandlers guards the single builtin tool
// table: every entry has a name and a handler, the plan-mode allowlist only
// references real tools, and the registration order matches the golden order.
// Order is significant because it is the order tools are serialized for the
// LLM provider, and provider prompt caches are keyed on that serialization.
func TestBuiltinToolNamesMatchSchemasAndHandlers(t *testing.T) {
	// Golden order — the cache-sensitive wire order. Append new tools at the
	// end of builtinToolDefs and here; never reorder existing entries.
	goldenOrder := []string{
		"list_files",
		"repo_overview",
		"glob",
		"read_file",
		"read_files",
		"list_definitions",
		"write_file",
		"execute_command",
		"replace_in_file",
		"delete",
		"patch_file",
		"show_diff",
		"search_code",
		"find_symbol",
		"git",
		"web_search",
		"web_fetch",
		"download_file",
		"git_commit",
		"git_stage",
		"todo",
		"find_file",
		"session_rename",
		"context_pin_last",
		"rename_symbol",
		"call_graph",
		"background_job",
		"read_image",
		"git_blame",
	}

	schemas := BuiltinTools()
	if len(schemas) != len(goldenOrder) {
		t.Fatalf("BuiltinTools() has %d tools, golden order has %d", len(schemas), len(goldenOrder))
	}
	schemaSet := make(map[string]struct{}, len(schemas))
	for i, tool := range schemas {
		if tool.Name != goldenOrder[i] {
			t.Errorf("tool %d: got %q, want %q (order is cache-sensitive)", i, tool.Name, goldenOrder[i])
		}
		if _, dup := schemaSet[tool.Name]; dup {
			t.Errorf("duplicate tool name %q", tool.Name)
		}
		schemaSet[tool.Name] = struct{}{}
	}
	for _, d := range builtinToolDefs {
		if d.Definition.Name == "" {
			t.Error("tool entry with empty name")
		}
		if d.Handler == nil {
			t.Errorf("tool %q has no handler", d.Definition.Name)
		}
	}
	for name := range planModeAllowedTools {
		if _, ok := schemaSet[name]; !ok {
			t.Errorf("planModeAllowedTools has unknown tool %q", name)
		}
	}
}

// TestPlanModeSuffixDerivedFromRegistry pins the derivation of the plan-mode
// suffix: the blocked-tool list must exactly match the tools the registry
// marks as neither ReadOnly nor PlanAllowed, so the prompt can never drift
// from the allowlist.
func TestPlanModeSuffixDerivedFromRegistry(t *testing.T) {
	if !strings.HasPrefix(planModePromptSuffix, "\n\nPlan mode is active.") {
		t.Fatalf("plan-mode suffix changed shape: %q", planModePromptSuffix)
	}
	const marker = "Do not call "
	start := strings.Index(planModePromptSuffix, marker)
	end := strings.Index(planModePromptSuffix, " tools.")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("plan-mode suffix lost its blocked-tool list: %q", planModePromptSuffix)
	}
	listed := strings.Split(planModePromptSuffix[start+len(marker):end], ", ")
	got := make(map[string]bool, len(listed))
	for _, n := range listed {
		got[n] = true
	}
	var want []string
	for _, d := range builtinToolDefs {
		if !d.ReadOnly && !d.PlanAllowed {
			want = append(want, d.Definition.Name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("suffix lists %d blocked tools, registry has %d: %q", len(got), len(want), planModePromptSuffix)
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("suffix missing blocked tool %q", n)
		}
	}
}
