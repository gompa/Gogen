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
<invoke name="glob_files">
<parameter name="pattern">*.go</parameter>
</invoke>
</function_calls>`,
			want:  "glob_files",
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
			name: "JSON without tool_call wrapper",
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
