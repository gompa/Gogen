package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestGenerateResponseStreamKeepsRefusalSeparate(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"refusal":"I cannot help with that."}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

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
	if result.Content != "" {
		t.Fatalf("Content should stay empty, got %q", result.Content)
	}
	if result.Refusal != "I cannot help with that." {
		t.Fatalf("Refusal = %q", result.Refusal)
	}
}

func TestGenerateResponseStreamKeepsReasoningOutOfContent(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"only thinking"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

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
	if result.Content != "" {
		t.Fatalf("Content should stay empty, got %q", result.Content)
	}
	if result.Reasoning != "only thinking" {
		t.Fatalf("Reasoning = %q", result.Reasoning)
	}
}

// TestGenerateResponseStreamReasoningStopGraceFires pins the bounded wait
// after a reasoning-only finish_reason="stop": a provider that sends the stop
// and then HOLDS the connection open (no [DONE], no more data, no close) must
// be treated as complete after reasoningStopGrace — returning the accumulated
// reasoning without a fallback re-request — instead of blocking for the full
// streamReadIdleTimeout. NOT parallel: it sets GOGEN_REASONING_STOP_GRACE.
func TestGenerateResponseStreamReasoningStopGraceFires(t *testing.T) {
	t.Setenv("GOGEN_REASONING_STOP_GRACE", "150ms")

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"reasoning_content":"only thinking"}}]}`+"\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fl.Flush()
		// Hold the connection open with no further data. The client's grace
		// timer closes the stream, which cancels this request context (the
		// handler returns so the test server can shut down). Bound the wait
		// in case the teardown does not propagate.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	start := time.Now()
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reasoning != "only thinking" {
		t.Fatalf("Reasoning = %q, want the accumulated reasoning", result.Reasoning)
	}
	if result.Content != "" {
		t.Fatalf("Content should stay empty, got %q", result.Content)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("grace did not bound the wait: elapsed %v", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the grace close must not trigger a fallback re-request)", got)
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

// TestGenerateResponseStreamCapturesReportedModel verifies the model ID the
// provider reports on the stream chunks (router endpoints such as OpenCode
// Zen resolve aliases server-side) is surfaced on the StreamResult.
func TestGenerateResponseStreamCapturesReportedModel(t *testing.T) {
	t.Parallel()
	const sse = `data: {"model":"glm-4.6","choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

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
	if result.Model != "glm-4.6" {
		t.Fatalf("result.Model = %q, want %q", result.Model, "glm-4.6")
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

// TestGenerateResponseStreamNoReportedModelIsEmpty verifies a stream whose
// chunks carry no model field yields an empty StreamResult.Model.
func TestGenerateResponseStreamNoReportedModelIsEmpty(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

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
	if result.Model != "" {
		t.Fatalf("result.Model = %q, want empty", result.Model)
	}
}

// TestStreamFallbackDefersRoundEndCallbacks pins the single-source-of-truth
// contract for round-end callbacks: when a failed stream is recovered via the
// non-streaming fallback, the LLM layer must NOT fire OnStreamEnd /
// OnRecoverPartialStream — the agent loop fires them exactly once when it
// finalizes the round. Firing them here too (the pre-fix behavior) delivered
// duplicate stream_end frames to the web client and duplicate
// streamRoundEndMsg events to the TUI on every stream failure.
func TestStreamFallbackDefersRoundEndCallbacks(t *testing.T) {
	t.Parallel()
	var streamed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamed = true
			w.Header().Set("Content-Type", "text/event-stream")
			// One valid chunk, then malformed SSE data: the stream errors
			// mid-way, forcing the non-streaming fallback.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var ends, recovers int
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnToken:                func(string) {},
			OnStreamEnd:            func() { ends++ },
			OnRecoverPartialStream: func() { recovers++ },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !streamed {
		t.Fatal("test setup: stream request never reached the server")
	}
	if ends != 0 || recovers != 0 {
		t.Fatalf("fallback fired round-end callbacks (OnStreamEnd=%d, OnRecoverPartialStream=%d); the agent loop owns these", ends, recovers)
	}
	if result.Content != "recovered answer" {
		t.Fatalf("fallback content = %q, want %q", result.Content, "recovered answer")
	}
	if !result.PartialStream {
		t.Fatal("PartialStream must be true: the failed stream produced partial content")
	}
}

// TestStreamFallbackTrimsAlreadyStreamedPrefix verifies the fallback does not
// re-render text the client already saw from the failed stream: when the
// recovered response starts with exactly the streamed prefix, only the suffix
// is emitted via OnToken, while the persisted StreamResult keeps the complete
// recovery.
func TestStreamFallbackTrimsAlreadyStreamedPrefix(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			// One valid chunk, then malformed SSE: the stream errors mid-way,
			// forcing the non-streaming fallback.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var tokens []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(token string) { tokens = append(tokens, token) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The streamed prefix fires OnToken once, and the fallback re-renders
	// only the suffix beyond it — the recovered text must not be emitted in
	// full on top of what the client already rendered.
	if len(tokens) != 2 || tokens[0] != "partial " || tokens[1] != "recovered answer" {
		t.Fatalf("OnToken fired %v, want [partial  recovered answer] (streamed prefix + trimmed suffix only)", tokens)
	}
	if result.Content != "partial recovered answer" {
		t.Fatalf("result.Content = %q, want %q (persisted result stays complete)", result.Content, "partial recovered answer")
	}
}

// TestStreamFallbackNoDuplicateWhenRecoveredEqualsStreamed verifies that when
// the recovered response is byte-identical to what the stream already
// emitted, nothing is re-rendered (there is nothing new to show).
func TestStreamFallbackNoDuplicateWhenRecoveredEqualsStreamed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"exact answer"}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"exact answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var tokens []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(token string) { tokens = append(tokens, token) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0] != "exact answer" {
		t.Fatalf("OnToken fired %v, want only the streamed [exact answer] (fallback must emit nothing new)", tokens)
	}
	if result.Content != "exact answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

// TestTrimRecoveredText pins the suffix-only trimming contract: only an exact
// byte prefix is dropped; divergent re-generations are emitted in full.
func TestTrimRecoveredText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		streamed  string
		recovered string
		want      string
	}{
		{"empty streamed", "", "answer", "answer"},
		{"empty recovered", "partial", "", ""},
		{"both empty", "", "", ""},
		{"exact prefix", "partial ", "partial recovered answer", "recovered answer"},
		{"identical", "exact", "exact", ""},
		{"divergent", "abc", "abx", "abx"},
		{"recovered shorter than streamed", "hello world", "hello", "hello"},
		{"utf8 prefix", "日", "日本語", "本語"},
		{"utf8 not a byte prefix", "本", "日本語", "日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimRecoveredText(tc.streamed, tc.recovered); got != tc.want {
				t.Fatalf("trimRecoveredText(%q, %q) = %q, want %q", tc.streamed, tc.recovered, got, tc.want)
			}
		})
	}
}

// TestStreamFallbackKeepsStreamedModel verifies the fallback StreamResult
// keeps the model ID the failed stream reported when the non-streaming
// response omits the model field.
func TestStreamFallbackKeepsStreamedModel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"model":"glm-4.6","choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// No "model" field: the fallback must fall back to the streamed model.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"recovered answer"}}]}`))
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
	if result.Model != "glm-4.6" {
		t.Fatalf("result.Model = %q, want %q", result.Model, "glm-4.6")
	}
}

// TestGenerateResponseStreamUsageAfterManyNoOps verifies the post-finish
// drain keeps consuming chunks until the usage chunk arrives, so a provider
// that sends many no-op chunks between finish_reason and the final usage
// chunk does not lose usage (the old 8-chunk drain bound dropped it).
func TestGenerateResponseStreamUsageAfterManyNoOps(t *testing.T) {
	t.Parallel()
	var sse strings.Builder
	sse.WriteString(`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n")
	for i := 0; i < 10; i++ {
		sse.WriteString("data: {}\n\n")
	}
	sse.WriteString(`data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse.String()))
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
	if result.Usage == nil {
		t.Fatal("result.Usage is nil: the usage chunk arriving after 10 no-op chunks was lost in the drain")
	}
	if result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 5 || result.Usage.TotalTokens != 15 {
		t.Fatalf("result.Usage = %#v, want prompt=10 completion=5 total=15", result.Usage)
	}
}
