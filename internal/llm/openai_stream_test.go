package llm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func newTestOpenAIProvider(srv *httptest.Server) *OpenAIProvider {
	return &OpenAIProvider{
		client: openai.NewClient(
			option.WithBaseURL(srv.URL),
			option.WithAPIKey("test"),
			option.WithHTTPClient(newSSEHTTPClient()),
		),
		model: "test-model",
	}
}

func TestGenerateResponseStreamThinkingKeepalive(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"index":0,"delta":{}}]}

data: {"choices":[{"delta":{"reasoning_content":" step two"}}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)

	var thinking []string
	var content []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnThinkingToken: func(token string) { thinking = append(thinking, token) },
			OnToken:         func(token string) { content = append(content, token) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(thinking, ""); got != "step one step two" {
		t.Fatalf("thinking = %q", got)
	}
	if got := strings.Join(content, ""); got != "answer" {
		t.Fatalf("content = %q", got)
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

func TestGenerateResponseStreamIgnoresSpuriousStopDuringReasoning(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"choices":[{"delta":{"reasoning_content":" step two"}}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)

	var thinking []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnThinkingToken: func(token string) { thinking = append(thinking, token) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(thinking, ""); got != "step one step two" {
		t.Fatalf("thinking = %q", got)
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

func TestGenerateResponseStreamTerminalToolSignal(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{}"}}]}}]}

data: {"choices":[{"index":0,"delta":{}}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_file" {
		t.Fatalf("toolCalls = %#v", result.ToolCalls)
	}
}

// TestGenerateResponseStreamIgnoresSpuriousStopOnReasoningChunk is the
// regression test for the real-world failure: llama.cpp emits a spurious
// finish_reason:"stop" on a chunk that ALSO carries a reasoning_content
// token. The old guard's `!deltaIsEmptyDelta(delta)` clause treated that as
// real content and terminated mid-reasoning, discarding the rest of the
// stream (more reasoning + the actual answer).
func TestGenerateResponseStreamIgnoresSpuriousStopOnReasoningChunk(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":" step two"},"finish_reason":"stop"}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)

	var thinking []string
	var content []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnThinkingToken: func(token string) { thinking = append(thinking, token) },
			OnToken:         func(token string) { content = append(content, token) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(thinking, ""); got != "step one step two" {
		t.Fatalf("thinking = %q", got)
	}
	if got := strings.Join(content, ""); got != "answer" {
		t.Fatalf("content = %q", got)
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

// TestGenerateResponseStreamTerminalToolSignalIgnoresKeepaliveBetweenArgs
// verifies that an empty {} keepalive chunk arriving between tool-argument
// fragments does NOT terminate the stream. The terminal-tool-signal branch
// must require the accumulated args to be complete JSON.
func TestGenerateResponseStreamTerminalToolSignalIgnoresKeepaliveBetweenArgs(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}

data: {"choices":[{"index":0,"delta":{}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{}}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %#v", result.ToolCalls)
	}
	tc := result.ToolCalls[0]
	if tc.Name != "read_file" {
		t.Fatalf("name = %q", tc.Name)
	}
	if tc.Args["path"] != "a.go" {
		t.Fatalf("args = %#v", tc.Args)
	}
}
