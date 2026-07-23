package llm

import (
	"strings"
	"testing"
)

func TestToolCallArgumentsJSONPrefersArgsStr(t *testing.T) {
	raw := `{"path": "a.go", "offset":1}`
	tc := ToolCall{
		Name:    "read_file",
		Args:    map[string]interface{}{"offset": 1.0, "path": "a.go"},
		ArgsStr: raw,
	}
	if got := toolCallArgumentsJSON(&tc); got != raw {
		t.Fatalf("got %q, want exact ArgsStr", got)
	}
}

func TestToolCallArgumentsJSONFallsBackToMarshal(t *testing.T) {
	tc := ToolCall{
		Name: "read_file",
		Args: map[string]interface{}{"path": "a.go"},
	}
	got := toolCallArgumentsJSON(&tc)
	if got != `{"path":"a.go"}` {
		t.Fatalf("got %q", got)
	}
	if tc.ArgsStr != got {
		t.Fatalf("ArgsStr not pinned: %q", tc.ArgsStr)
	}
}

func TestToolCallArgumentsJSONFallsBackOnInvalidArgsStr(t *testing.T) {
	orig := `{"path":`
	tc := ToolCall{
		Name:    "read_file",
		Args:    map[string]interface{}{"path": "a.go"},
		ArgsStr: orig,
	}
	if got := toolCallArgumentsJSON(&tc); got != `{"path":"a.go"}` {
		t.Fatalf("got %q", got)
	}
	// Invalid provider fragment must stay in history — remarsal is wire-only.
	if tc.ArgsStr != orig {
		t.Fatalf("ArgsStr overwritten: %q", tc.ArgsStr)
	}
}

func TestToolCallArgumentsJSONDoesNotHTMLEscape(t *testing.T) {
	tc := ToolCall{
		Name: "read_file",
		Args: map[string]interface{}{"path": "a<b>.go"},
	}
	got := toolCallArgumentsJSON(&tc)
	if strings.Contains(got, `\u003c`) {
		t.Fatalf("HTML-escaped remarsal breaks cache stability: %q", got)
	}
	if got != `{"path":"a<b>.go"}` {
		t.Fatalf("got %q", got)
	}
}

func TestToolCallArgumentsJSONNormalizesWhitespace(t *testing.T) {
	tc := ToolCall{
		Name:    "read_file",
		ArgsStr: "  {\"path\":\"a.go\"}  \n",
	}
	got := toolCallArgumentsJSON(&tc)
	if got != `{"path":"a.go"}` {
		t.Fatalf("got %q", got)
	}
	if tc.ArgsStr != got {
		t.Fatalf("ArgsStr not normalized in place: %q", tc.ArgsStr)
	}
	// Second call must be identical (prompt-cache prefix stability).
	if got2 := toolCallArgumentsJSON(&tc); got2 != got {
		t.Fatalf("second call %q != first %q", got2, got)
	}
}

func TestToolCallArgumentsJSONPinsEmptyObject(t *testing.T) {
	tc := ToolCall{Name: "noop"}
	if got := toolCallArgumentsJSON(&tc); got != "{}" {
		t.Fatalf("got %q", got)
	}
	if tc.ArgsStr != "{}" {
		t.Fatalf("nil Args should pin ArgsStr to {}, got %q", tc.ArgsStr)
	}
}

func TestToolCallArgumentsJSONStableAcrossCalls(t *testing.T) {
	tc := ToolCall{
		Name: "read_file",
		Args: map[string]interface{}{"z": 1.0, "a": 2.0, "m": 3.0},
	}
	first := toolCallArgumentsJSON(&tc)
	second := toolCallArgumentsJSON(&tc)
	if first != second {
		t.Fatalf("unstable args json: %q vs %q", first, second)
	}
	if first != `{"a":2,"m":3,"z":1}` {
		t.Fatalf("got %q, want sorted-key marshal", first)
	}
}
