package llm

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGenerateResponseMalformedToolArgsPerCall pins that the non-streaming
// path records a malformed tool-call arguments blob per call (ToolCall.ArgsError)
// and still returns the remaining tool calls, matching the streaming
// accumulator (buildResult) so recovery does not depend on which transport
// served the turn.
func TestGenerateResponseMalformedToolArgsPerCall(t *testing.T) {
	t.Parallel()
	const body = `{
		"id": "chatcmpl-1",
		"object": "chat.completion",
		"created": 1,
		"model": "test-model",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"a.go\"}"}},
					{"id": "call_2", "type": "function", "function": {"name": "read_file", "arguments": "{not json"}}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)

	resp, err := p.GenerateResponse(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("GenerateResponse returned error for one malformed call: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(resp.ToolCalls))
	}

	good := resp.ToolCalls[0]
	if good.Name != "read_file" || good.ArgsError != "" {
		t.Fatalf("first call = %+v, want valid read_file", good)
	}
	if good.Args["path"] != "a.go" {
		t.Fatalf("first call args = %v, want path=a.go", good.Args)
	}

	bad := resp.ToolCalls[1]
	if bad.Name != "read_file" {
		t.Fatalf("second call name = %q, want read_file", bad.Name)
	}
	if bad.ArgsError == "" {
		t.Fatalf("second call ArgsError empty, want parse error")
	}
	if len(bad.Args) != 0 {
		t.Fatalf("second call args = %v, want empty", bad.Args)
	}
	if bad.ArgsStr != "{not json" {
		t.Fatalf("second call ArgsStr = %q, want raw provider bytes", bad.ArgsStr)
	}
}
