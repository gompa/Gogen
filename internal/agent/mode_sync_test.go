package agent

import "testing"

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
		"glob_files",
		"read_file",
		"read_files",
		"list_definitions",
		"write_file",
		"execute_command",
		"run_tests",
		"run_lint",
		"replace_in_file",
		"delete_file",
		"move_file",
		"patch_file",
		"show_diff",
		"search_code",
		"find_references",
		"git_log",
		"git_blame",
		"git_status",
		"web_search",
		"web_fetch",
		"download_file",
		"git_commit",
		"git_stage",
		"git_branch",
		"git_stash",
		"git_stash_list",
		"git_show",
		"copy_file",
		"todo_add",
		"todo_list",
		"todo_done",
		"todo_remove",
		"todo_clear_done",
		"find_file",
		"find_definition",
		"session_rename",
		"session_usage",
		"context_pin_last",
		"context_pins",
		"rename_symbol",
		"multi_edit",
		"call_graph",
		"dependency_analysis",
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
