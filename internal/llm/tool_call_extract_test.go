package llm

import (
	"testing"
)

func TestExtractToolCallsFromText_Formats(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // expected tool name (empty if none expected)
		wantN int    // expected number of tool calls
	}{
		{
			name: "equals-sign format (user's example)",
			input: `<thinking>
<tool_call>
<function=list_files>
<parameter=path>
/home/gompa/workspace/Gogen/internal
</parameter>
<parameter=recursive>
true
</parameter>
</function>
</tool_call>
</thinking>`,
			want:  "list_files",
			wantN: 1,
		},
		{
			name: "proper XML with function content and parameters JSON",
			input: `<thinking>
<tool_call>
<function>list_files</function>
<parameters>{"path": "/internal", "recursive": true}</parameters>
</tool_call>
</thinking>`,
			want:  "list_files",
			wantN: 1,
		},
		{
			name: "JSON inside tool_call block",
			input: `<thinking>
<tool_call>
{"name": "read_file", "arguments": {"file_path": "/x.go"}}
</tool_call>
</thinking>`,
			want:  "read_file",
			wantN: 1,
		},
		{
			name: "function attribute format",
			input: `<tool_call>
<function name="search_code">
<parameter name="pattern">foo</parameter>
<parameter name="path">./internal</parameter>
</function>
</tool_call>`,
			want:  "search_code",
			wantN: 1,
		},
		{
			name: "Anthropic invoke format",
			input: `<function_calls>
<invoke name="glob">
<parameter name="pattern">*.go</parameter>
</invoke>
</function_calls>`,
			want:  "glob",
			wantN: 1,
		},
		{
			name: "tool_name tag format",
			input: `<tool_call>
<tool_name>execute_command</tool_name>
<parameters>{"command": "ls"}</parameters>
</tool_call>`,
			want:  "execute_command",
			wantN: 1,
		},
		{
			name:  "JSON without tool_call wrapper",
			input: `I'll use {"name": "list_files", "arguments": {"path": "/tmp"}}`,
			want:  "list_files",
			wantN: 1,
		},
		{
			name:  "no tool calls",
			input: `Just some regular text without any tool calls.`,
			want:  "",
			wantN: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := extractToolCallsFromText(tt.input)
			if len(calls) != tt.wantN {
				t.Fatalf("got %d tool calls, want %d", len(calls), tt.wantN)
			}
			if tt.wantN > 0 {
				if calls[0].Name != tt.want {
					t.Fatalf("tool name = %q, want %q", calls[0].Name, tt.want)
				}
				t.Logf("name=%q args=%v argsStr=%q", calls[0].Name, calls[0].Args, calls[0].ArgsStr)
			}
		})
	}
}

func TestExtractToolCallsFromText_EqualsSignParams(t *testing.T) {
	input := `<tool_call>
<function=list_files>
<parameter=path>
/home/gompa/workspace/Gogen/internal
</parameter>
<parameter=recursive>
true
</parameter>
</function>
</tool_call>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	tc := calls[0]
	if tc.Name != "list_files" {
		t.Fatalf("name = %q, want list_files", tc.Name)
	}
	if tc.Args["path"] != "/home/gompa/workspace/Gogen/internal" {
		t.Fatalf("path = %q", tc.Args["path"])
	}
	if tc.Args["recursive"] != true {
		t.Fatalf("recursive = %v, want true", tc.Args["recursive"])
	}
}

func TestExtractToolCallsFromText_MultipleCalls(t *testing.T) {
	input := `<tool_call>
<function>read_file</function>
<parameters>{"file_path": "/a.go"}</parameters>
</tool_call>
<tool_call>
<function>search_code</function>
<parameters>{"pattern": "foo"}</parameters>
</tool_call>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("call 0 = %q", calls[0].Name)
	}
	if calls[1].Name != "search_code" {
		t.Fatalf("call 1 = %q", calls[1].Name)
	}
}

func TestExtractToolCallsFromText_MultipleJSONInOneBlock(t *testing.T) {
	input := `<tool_call>
{"name": "read_file", "arguments": {"file_path": "/a.go"}}
{"name": "search_code", "arguments": {"pattern": "foo"}}
</tool_call>`

	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2; calls=%+v", len(calls), calls)
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("call 0 = %q, want read_file", calls[0].Name)
	}
	if calls[1].Name != "search_code" {
		t.Fatalf("call 1 = %q, want search_code", calls[1].Name)
	}
	if calls[0].ID == calls[1].ID {
		t.Fatal("expected distinct tool call IDs")
	}
}

// TestExtractToolCallsMultiBlockIDsDistinct verifies that JSON tool calls in
// SEPARATE <tool_call> blocks get globally unique IDs and indices (each block
// used to renumber from its own local counter, so every block's first call
// collided on "tc_extracted_0" — a duplicate tool-call ID the agent would
// echo back in tool results).
func TestExtractToolCallsMultiBlockIDsDistinct(t *testing.T) {
	input := `<tool_call>
{"name": "read_file", "arguments": {"path": "/a.go"}}
</tool_call>
<tool_call>
{"name": "search_code", "arguments": {"pattern": "foo"}}
</tool_call>`
	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID == calls[1].ID {
		t.Fatalf("tool call IDs must be distinct across blocks, both %q", calls[0].ID)
	}
	if calls[0].Index == calls[1].Index {
		t.Fatalf("tool call indices must be distinct across blocks, both %d", calls[0].Index)
	}
	if calls[0].ID == "" || calls[1].ID == "" {
		t.Fatal("tool call IDs must not be empty")
	}
}

// TestExtractToolCallsMultiInvokeIDsDistinct verifies the same uniqueness
// for separate <invoke> blocks (Anthropic style).
func TestExtractToolCallsMultiInvokeIDsDistinct(t *testing.T) {
	input := `<invoke name="glob">
<parameter name="pattern">*.go</parameter>
</invoke>
<invoke name="list_files">
<parameter name="path">/tmp</parameter>
</invoke>`
	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID == calls[1].ID {
		t.Fatalf("tool call IDs must be distinct across invoke blocks, both %q", calls[0].ID)
	}
	if calls[0].Index == calls[1].Index {
		t.Fatalf("tool call indices must be distinct across invoke blocks, both %d", calls[0].Index)
	}
}

// TestExtractToolCallsMixedXMLAndJSON verifies that a bare JSON tool-call
// object AFTER an XML-wrapped block is still extracted (the old
// len(toolCalls)==0 gate silently dropped it).
func TestExtractToolCallsMixedXMLAndJSON(t *testing.T) {
	input := `<tool_call>
{"name": "read_file", "arguments": {"path": "/a.go"}}
</tool_call>
{"name": "search_code", "arguments": {"pattern": "foo"}}`
	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (block + bare JSON), got %d: %+v", len(calls), calls)
	}
	names := map[string]bool{}
	for _, c := range calls {
		names[c.Name] = true
	}
	if !names["read_file"] || !names["search_code"] {
		t.Fatalf("expected read_file and search_code, got %+v", calls)
	}
}

// TestExtractToolCallsJSONInsideBlockNotDuplicated verifies the always-on
// text-level JSON scan skips objects already parsed inside <tool_call>
// blocks instead of double-extracting them.
func TestExtractToolCallsJSONInsideBlockNotDuplicated(t *testing.T) {
	input := `<tool_call>
{"name": "read_file", "arguments": {"path": "/a.go"}}
</tool_call>
{"name": "search_code", "arguments": {"pattern": "foo"}}`
	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (no duplicate of the in-block object), got %d: %+v", len(calls), calls)
	}
	counts := map[string]int{}
	for _, c := range calls {
		counts[c.Name]++
	}
	if counts["read_file"] != 1 || counts["search_code"] != 1 {
		t.Fatalf("expected exactly one of each call, got %+v", counts)
	}
}

// TestParseParamValueLargeIntExact pins that integer parameter values beyond
// float64's exact range (2^53) round-trip exactly instead of being silently
// rounded to a neighboring value (regression: 9007199254740993 became
// 9007199254740992). Small integers keep the historical float64 shape.
func TestParseParamValueLargeIntExact(t *testing.T) {
	big := int64(9007199254740993) // 2^53 + 1
	got := parseParamValue("9007199254740993")
	iv, ok := got.(int64)
	if !ok {
		t.Fatalf("large integer parsed as %T (%v), want int64", got, got)
	}
	if iv != big {
		t.Fatalf("large integer = %d, want %d (precision lost)", iv, big)
	}
	// 2^53 itself is exactly representable as float64 — keep the shape.
	got = parseParamValue("9007199254740992")
	fv, ok := got.(float64)
	if !ok {
		t.Fatalf("2^53 parsed as %T (%v), want float64", got, got)
	}
	if fv != 9007199254740992 {
		t.Fatalf("2^53 = %v, want 9007199254740992", fv)
	}
	// Small integers and non-integers keep the historical shapes.
	if v := parseParamValue("42"); v != float64(42) {
		t.Fatalf("42 parsed as %T (%v), want float64(42)", v, v)
	}
	if v := parseParamValue("3.5"); v != float64(3.5) {
		t.Fatalf("3.5 parsed as %T (%v), want float64(3.5)", v, v)
	}
	if v := parseParamValue("true"); v != true {
		t.Fatalf("true parsed as %T (%v), want bool true", v, v)
	}
}

// TestExtractToolCallsBareJSONStrictOnly pins the bare-JSON acceptance rule:
// an object is only treated as a tool call when it carries an arguments/input
// container. Name-only objects, flattened shapes, and prose JSON such as
// {"name": "service", "version": 2} must NOT be extracted — they used to be
// mis-executed as phantom tool calls.
func TestExtractToolCallsBareJSONStrictOnly(t *testing.T) {
	tests := []struct {
		name  string
		input string
		wantN int
	}{
		{
			name:  "arguments container accepted",
			input: `I'll use {"name": "list_files", "arguments": {"path": "/tmp"}}`,
			wantN: 1,
		},
		{
			name:  "input container accepted",
			input: `{"name": "read_file", "input": {"path": "/a.go"}}`,
			wantN: 1,
		},
		{
			name:  "name-only object rejected",
			input: `I could use {"name": "list_files"} here.`,
			wantN: 0,
		},
		{
			name:  "prose config json rejected",
			input: `The config is {"name": "service", "version": 2}.`,
			wantN: 0,
		},
		{
			name:  "prose person json rejected",
			input: `The user is {"name": "John", "age": 30}.`,
			wantN: 0,
		},
		{
			name:  "flattened shape rejected",
			input: `{"name": "read_file", "path": "/a.go"}`,
			wantN: 0,
		},
		{
			name:  "array-element object rejected",
			input: `{"results": [{"name": "list_files", "arguments": {"path": "/tmp"}}]}`,
			wantN: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := extractToolCallsFromText(tt.input)
			if len(calls) != tt.wantN {
				t.Fatalf("got %d calls, want %d: %+v", len(calls), tt.wantN, calls)
			}
		})
	}
}

// TestExtractToolCallsBareJSONBesideXMLBlock pins the strict tier beside
// explicit blocks: a bare JSON call WITH arguments after an XML block is
// still extracted, while prose config JSON or a name-only object beside a
// block is not.
func TestExtractToolCallsBareJSONBesideXMLBlock(t *testing.T) {
	block := `<tool_call>
{"name": "read_file", "arguments": {"path": "/a.go"}}
</tool_call>
`
	// Bare JSON call with arguments: extracted alongside the block.
	input := block + `{"name": "search_code", "arguments": {"pattern": "foo"}}`
	calls := extractToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (block + bare with arguments), got %d: %+v", len(calls), calls)
	}

	// Prose config JSON beside the block: NOT extracted.
	input = block + `The config is {"name": "service", "version": 2}.`
	calls = extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (block only), got %d: %+v", len(calls), calls)
	}

	// Name-only object beside the block: NOT extracted.
	input = block + `I could also use {"name": "list_files"}.`
	calls = extractToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (block only), got %d: %+v", len(calls), calls)
	}
}

// TestExtractToolCallsFromTextModeNoBareJSON pins that mode=false extracts
// explicit <tool_call> blocks but ignores bare JSON objects — the mode used
// for reasoning segments, where JSON is routinely drafted or quoted.
func TestExtractToolCallsFromTextModeNoBareJSON(t *testing.T) {
	// Bare JSON in reasoning-like text: ignored.
	calls := extractToolCallsFromTextMode(`I'll use {"name": "list_files", "arguments": {"path": "/tmp"}}`, false)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls from bare JSON with allowBareJSON=false, got %d: %+v", len(calls), calls)
	}
	// XML blocks in reasoning-like text: still extracted.
	calls = extractToolCallsFromTextMode(`<tool_call>
{"name": "read_file", "arguments": {"path": "/a.go"}}
</tool_call>`, false)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call from block with allowBareJSON=false, got %d: %+v", len(calls), calls)
	}
}
