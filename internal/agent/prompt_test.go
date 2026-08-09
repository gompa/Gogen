package agent

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestSystemPromptTemplate pins the prompt guidance that models rely on:
// the MCP hint, the parallel-batching rule, tool-output authority, and the
// compaction/todo guidance. These are the additions that make the template
// more powerful without bloating it.
func TestSystemPromptTemplate(t *testing.T) {
	p := SystemPrompt("/tmp/project")
	for _, want := range []string{
		"mcp_*",            // MCP tools may be present
		"at most 4",        // parallel read-only cap
		"ground truth",     // tool output is authoritative
		"context_pin_last", // survive compaction
		"todo",             // task tracking
	} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestReadFileSchemaDropsDeprecatedAlias pins the schema cleanup: the
// deprecated file_path alias is gone from the LLM-facing schema and path is
// required (it is now the only way to name a file).
func TestReadFileSchemaDropsDeprecatedAlias(t *testing.T) {
	for _, d := range builtinToolDefs {
		if d.Definition.Name != "read_file" {
			continue
		}
		props, ok := d.Definition.Parameters["properties"].(map[string]interface{})
		if !ok {
			t.Fatal("read_file schema has no properties")
		}
		if _, ok := props["file_path"]; ok {
			t.Error("read_file schema still advertises deprecated file_path alias")
		}
		if _, ok := props["path"]; !ok {
			t.Error("read_file schema missing path")
		}
		req, ok := d.Definition.Parameters["required"].([]string)
		if !ok || len(req) != 1 || req[0] != "path" {
			t.Errorf("read_file required = %v, want [path]", req)
		}
		return
	}
	t.Fatal("read_file tool not found")
}

// TestActionToolsHaveEnums pins the enum constraint on action/kind parameters
// of the consolidated tools, so invalid values fail fast instead of
// round-tripping through handlers.
func TestActionToolsHaveEnums(t *testing.T) {
	byName := make(map[string]llm.Tool, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		byName[d.Definition.Name] = d.Definition
	}
	cases := map[string]string{
		"git":            "action",
		"find_symbol":    "kind",
		"background_job": "action",
		"todo":           "action",
	}
	for tool, param := range cases {
		def, ok := byName[tool]
		if !ok {
			t.Fatalf("tool %q missing from registry", tool)
		}
		props, ok := def.Parameters["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: no properties", tool)
		}
		p, ok := props[param].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: missing %s param", tool, param)
		}
		if _, ok := p["enum"]; !ok {
			t.Errorf("%s: %s param has no enum", tool, param)
		}
	}
}
