package llm

import "testing"

// TestTodoToolCallKeepsID pins that tool-call extraction preserves the todo
// "id" argument in every format the model can emit: JSON tool-call objects
// (inside <tool_call> blocks and standalone), Anthropic-style <invoke> with
// <parameter> tags, and equals-sign parameters. A dropped id here would
// surface as "todo done missing its id" downstream.
func TestTodoToolCallKeepsID(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"json in tool_call block", `<tool_call>{"name":"todo","arguments":{"action":"done","id":1}}</tool_call>`},
		{"standalone json", `{"name":"todo","arguments":{"action":"done","id":1}}`},
		{"invoke with parameter tags", `<invoke name="todo"><parameter name="action">done</parameter><parameter name="id">1</parameter></invoke>`},
		{"equals-sign parameters", `<tool_call><function name="todo"><parameter=action>done</parameter><parameter=id>1</parameter></function></tool_call>`},
		{"string id preserved", `<tool_call>{"name":"todo","arguments":{"action":"done","id":"1"}}</tool_call>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := extractToolCallsFromText(tc.input)
			if len(calls) != 1 {
				t.Fatalf("expected 1 tool call, got %d", len(calls))
			}
			c := calls[0]
			if c.Name != "todo" {
				t.Fatalf("name = %q, want todo", c.Name)
			}
			action, ok := c.Args["action"]
			if !ok || action != "done" {
				t.Fatalf("action = %#v, want done", c.Args["action"])
			}
			if _, ok := c.Args["id"]; !ok {
				t.Fatalf("id dropped from tool call args: %#v", c.Args)
			}
		})
	}
}
