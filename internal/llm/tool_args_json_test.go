package llm

import (
	"strings"
	"testing"
)

func TestToolCallArgumentsJSONPrefersArgsStr(t *testing.T) {
	raw := `{"path": "a.go", "offset":1}`
	tc := ToolCall{
		Name:    "read_file",
		Args:    map[string]any{"offset": 1.0, "path": "a.go"},
		ArgsStr: raw,
	}
	if got := toolCallArgumentsJSON(&tc); got != raw {
		t.Fatalf("got %q, want exact ArgsStr", got)
	}
}

func TestToolCallArgumentsJSONFallsBackToMarshal(t *testing.T) {
	tc := ToolCall{
		Name: "read_file",
		Args: map[string]any{"path": "a.go"},
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
		Args:    map[string]any{"path": "a.go"},
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
		Args: map[string]any{"path": "a<b>.go"},
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
		Args: map[string]any{"z": 1.0, "a": 2.0, "m": 3.0},
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

// TestStabilizeToolCallArgsMemoizesValidity verifies StabilizeToolCallArgs
// records ArgsJSONValid for valid ArgsStr so the wire serializer skips the
// per-request json.Valid scan, while an invalid ArgsStr stays un-flagged so
// the serializer keeps remarsaling for the wire (without overwriting
// history) exactly as before.
func TestStabilizeToolCallArgsMemoizesValidity(t *testing.T) {
	valid := ToolCall{Name: "read_file", ArgsStr: `{"path":"a.go"}`}
	StabilizeToolCallArgs(&valid)
	if !valid.ArgsJSONValid {
		t.Fatal("valid ArgsStr should be flagged ArgsJSONValid")
	}
	// The serializer takes the fast path and returns the exact bytes.
	if got := toolCallArgumentsJSON(&valid); got != `{"path":"a.go"}` {
		t.Fatalf("fast path returned %q", got)
	}

	invalid := ToolCall{Name: "read_file", ArgsStr: `{"path":`, Args: map[string]any{"path": "a.go"}}
	StabilizeToolCallArgs(&invalid)
	if invalid.ArgsJSONValid {
		t.Fatal("invalid ArgsStr must not be flagged valid")
	}
	// Serializer still remarsals for the wire without touching ArgsStr.
	if got := toolCallArgumentsJSON(&invalid); got != `{"path":"a.go"}` {
		t.Fatalf("invalid-path serialization = %q", got)
	}
	if invalid.ArgsStr != `{"path":` {
		t.Fatalf("invalid ArgsStr overwritten: %q", invalid.ArgsStr)
	}

	empty := ToolCall{Name: "noop"}
	StabilizeToolCallArgs(&empty)
	if !empty.ArgsJSONValid || empty.ArgsStr != "{}" {
		t.Fatalf("empty args should stabilize to pinned {} and be flagged: %q valid=%v", empty.ArgsStr, empty.ArgsJSONValid)
	}
}
