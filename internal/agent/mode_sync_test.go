package agent

import "testing"

func TestBuiltinToolNamesMatchSchemasAndHandlers(t *testing.T) {
	schemas := BuiltinTools()
	handlers := BuiltinToolHandlers()
	if len(builtinToolNames) != len(schemas) {
		t.Fatalf("builtinToolNames=%d schemas=%d", len(builtinToolNames), len(schemas))
	}
	schemaSet := make(map[string]struct{}, len(schemas))
	for _, tool := range schemas {
		schemaSet[tool.Name] = struct{}{}
		if _, ok := handlers[tool.Name]; !ok {
			t.Errorf("missing handler for schema tool %q", tool.Name)
		}
	}
	for _, name := range builtinToolNames {
		if _, ok := schemaSet[name]; !ok {
			t.Errorf("builtinToolNames has %q not in BuiltinTools()", name)
		}
	}
	for name := range handlers {
		if _, ok := schemaSet[name]; !ok {
			t.Errorf("handler %q has no BuiltinTools() schema", name)
		}
	}
	for name := range planModeAllowedTools {
		if _, ok := schemaSet[name]; !ok {
			t.Errorf("planModeAllowedTools has unknown tool %q", name)
		}
	}
}
