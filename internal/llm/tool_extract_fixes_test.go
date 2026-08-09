package llm

import (
	"testing"
)

// TestExtractToolCallsNoDoubleInvoke verifies that <invoke> inside <tool_call>
// is not extracted twice.
func TestExtractToolCallsNoDoubleInvoke(t *testing.T) {
	input := `<thinking>
<tool_call>
<invoke name="list_files">
<parameter name="path">/tmp</parameter>
</invoke>
</tool_call>
</thinking>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call (deduped), got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "list_files" {
		t.Fatalf("expected list_files, got %q", calls[0].Name)
	}
}

// TestExtractToolCallsInvokeOutsideToolCall verifies standalone <invoke> still works.
func TestExtractToolCallsInvokeOutsideToolCall(t *testing.T) {
	input := `<invoke name="glob">
<parameter name="pattern">*.go</parameter>
</invoke>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "glob" {
		t.Fatalf("expected glob, got %q", calls[0].Name)
	}
}

// TestExtractToolCallsNestedJSONArgs verifies that JSON args with nested
// objects are properly extracted (the flat regex would have failed).
func TestExtractToolCallsNestedJSONArgs(t *testing.T) {
	input := `<tool_call>
<function>read_file</function>
<parameters>{"path": "file.go", "options": {"offset": 5, "limit": 10, "search": true}}</parameters>
</tool_call>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("expected read_file, got %q", calls[0].Name)
	}
	opts, ok := calls[0].Args["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("options should be a map, got %T: %v", calls[0].Args["options"], calls[0].Args)
	}
	if opts["offset"] != float64(5) {
		t.Fatalf("options.offset mismatch: %v", opts["offset"])
	}
}

// TestParseToolCallFromJSONStringID verifies IDs are distinct for multiple calls.
func TestParseToolCallFromJSONStringIDsDistinct(t *testing.T) {
	input := `<tool_call>
{"name": "read_file", "arguments": {"file_path": "/a.go"}}
{"name": "search_code", "arguments": {"pattern": "foo"}}
</tool_call>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID == calls[1].ID {
		t.Fatal("tool call IDs must be distinct")
	}
	if calls[0].ID == "" || calls[1].ID == "" {
		t.Fatal("tool call IDs must not be empty")
	}
}

// TestExtractToolCallsNestedJSONFallback verifies that the fallback path
// handles nested JSON objects (not just flat \{[^{}]*\}).
func TestExtractToolCallsNestedJSONFallback(t *testing.T) {
	input := `<tool_call>
{"name": "search_code", "arguments": {"pattern": "func", "glob": "*.go", "context": {"before": 2, "after": 3}}}
</tool_call>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "search_code" {
		t.Fatalf("expected search_code, got %q", calls[0].Name)
	}
	ctx, ok := calls[0].Args["context"].(map[string]interface{})
	if !ok {
		t.Fatalf("context should be a map, got %T: %v", calls[0].Args["context"], calls[0].Args)
	}
	if ctx["before"] != float64(2) || ctx["after"] != float64(3) {
		t.Fatalf("context mismatch: %v", ctx)
	}
}

// TestExtractToolCallsStandaloneJSON verifies raw JSON (no XML wrapper) still works.
func TestExtractToolCallsStandaloneJSONNested(t *testing.T) {
	input := `I'll use {"name": "list_files", "arguments": {"path": "/tmp", "filter": {"hidden": false, "max": 5}}}`

	calls := extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "list_files" {
		t.Fatalf("expected list_files, got %q", calls[0].Name)
	}
	filter, ok := calls[0].Args["filter"].(map[string]interface{})
	if !ok {
		t.Fatalf("filter should be a map, got %T: %v", calls[0].Args["filter"], calls[0].Args)
	}
	if filter["hidden"] != false || filter["max"] != float64(5) {
		t.Fatalf("filter mismatch: %v", filter)
	}
}
