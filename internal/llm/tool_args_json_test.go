package llm

import "testing"

func TestToolCallArgumentsJSONPrefersArgsStr(t *testing.T) {
	raw := `{"path": "a.go", "offset":1}`
	tc := ToolCall{
		Name:    "read_file",
		Args:    map[string]interface{}{"offset": 1.0, "path": "a.go"},
		ArgsStr: raw,
	}
	if got := toolCallArgumentsJSON(tc); got != raw {
		t.Fatalf("got %q, want exact ArgsStr", got)
	}
}

func TestToolCallArgumentsJSONFallsBackToMarshal(t *testing.T) {
	tc := ToolCall{
		Name: "read_file",
		Args: map[string]interface{}{"path": "a.go"},
	}
	got := toolCallArgumentsJSON(tc)
	if got != `{"path":"a.go"}` {
		t.Fatalf("got %q", got)
	}
}

func TestToolCallArgumentsJSONFallsBackOnInvalidArgsStr(t *testing.T) {
	tc := ToolCall{
		Name:    "read_file",
		Args:    map[string]interface{}{"path": "a.go"},
		ArgsStr: `{"path":`,
	}
	if got := toolCallArgumentsJSON(tc); got != `{"path":"a.go"}` {
		t.Fatalf("got %q", got)
	}
}
