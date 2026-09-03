package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go"
)

func TestApplyChatCompletionExtrasUsesPropsCapability(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	var gotModel atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		gotModel.Store(r.URL.Query().Get("model"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]bool{
				"supports_preserve_reasoning": true,
			},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	if err := p.SetModel("test-model"); err != nil {
		t.Fatal(err)
	}
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	if got := gotModel.Load(); got != "test-model" {
		t.Fatalf("probe ?model= %v, want test-model", got)
	}

	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	kwargs, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing: %s", b)
	}
	if kwargs["preserve_reasoning"] != true {
		t.Fatalf("preserve_reasoning = %#v, want true", kwargs["preserve_reasoning"])
	}

	// Second call must use the cache (still one /props hit).
	params2 := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params2)
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want 1", got)
	}
}

func TestApplyChatCompletionExtrasSkippedWhenCapsFalse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]bool{
				"supports_preserve_reasoning": false,
			},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["chat_template_kwargs"]; ok {
		t.Fatalf("should omit kwargs when capability is false, got %s", b)
	}
}

func TestApplyChatCompletionExtrasSkippedWhenPropsMissing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["chat_template_kwargs"]; ok {
		t.Fatalf("should omit kwargs when /props is unavailable, got %s", b)
	}
}

func TestApplyChatCompletionExtrasSkippedWithoutProps(t *testing.T) {
	t.Parallel()

	// Default OpenAI (empty base URL): never probe, never send.
	p := &OpenAIProvider{}
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	assertNoChatTemplateKwargs(t, params)

	// Custom URL without /props capability: auto stays quiet.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p2 := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	params2 := openai.ChatCompletionNewParams{Model: "test-model"}
	p2.applyChatCompletionExtras(context.Background(), &params2)
	assertNoChatTemplateKwargs(t, params2)
}

func TestApplyChatCompletionExtrasForcedOnSkipsProps(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	p.SetPreserveReasoningMode("on")
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	kwargs, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing: %s", b)
	}
	if kwargs["preserve_reasoning"] != true {
		t.Fatalf("preserve_reasoning = %#v, want true", kwargs["preserve_reasoning"])
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("/props hits = %d, want 0 when forced on", got)
	}
}

func TestApplyChatCompletionExtrasForcedOff(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]bool{"supports_preserve_reasoning": true},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	p.SetPreserveReasoningMode("off")
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["chat_template_kwargs"]; ok {
		t.Fatalf("should omit kwargs when forced off, got %s", b)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("/props hits = %d, want 0 when forced off", got)
	}
}

func TestApplyChatCompletionExtrasForcedOnRequiresBaseURL(t *testing.T) {
	t.Parallel()
	p := &OpenAIProvider{}
	p.SetPreserveReasoningMode("on")
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	assertNoChatTemplateKwargs(t, params)
}

func assertNoChatTemplateKwargs(t *testing.T, params openai.ChatCompletionNewParams) {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["chat_template_kwargs"]; ok {
		t.Fatalf("should omit chat_template_kwargs, got %s", b)
	}
}

func TestNormalizePreserveReasoningMode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":      "auto",
		"auto":  "auto",
		"on":    "on",
		"ON":    "on",
		"true":  "on",
		"1":     "on",
		"off":   "off",
		"false": "off",
		"0":     "off",
		"bogus": "auto",
	}
	for in, want := range cases {
		if got := normalizePreserveReasoningMode(in); got != want {
			t.Fatalf("normalizePreserveReasoningMode(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestSetModelInvalidatesPropsCaps(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]bool{"supports_preserve_reasoning": true},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}
	_ = p.templateSupportsPreserveReasoning(context.Background())
	if err := p.SetModel("other"); err != nil {
		t.Fatal(err)
	}
	_ = p.templateSupportsPreserveReasoning(context.Background())
	if got := hits.Load(); got != 2 {
		t.Fatalf("/props hits after SetModel = %d, want 2", got)
	}
}

func TestLlamaPropsURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		base, model, want string
	}{
		{"http://127.0.0.1:8080/v1", "", "http://127.0.0.1:8080/props"},
		{"http://127.0.0.1:8080/v1/", "", "http://127.0.0.1:8080/props"},
		{"http://127.0.0.1:8080", "", "http://127.0.0.1:8080/props"},
		{"", "", ""},
		{"http://127.0.0.1:8080/v1", "qwen3.8", "http://127.0.0.1:8080/props?model=qwen3.8"},
		{"http://127.0.0.1:8080/v1", "my model", "http://127.0.0.1:8080/props?model=my+model"},
	}
	for _, tc := range cases {
		if got := llamaPropsURL(tc.base, tc.model); got != tc.want {
			t.Fatalf("llamaPropsURL(%q, %q)=%q, want %q", tc.base, tc.model, got, tc.want)
		}
	}
}

func TestLlamaApplyTemplateURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		base, model, want string
	}{
		{"http://127.0.0.1:8080/v1", "", "http://127.0.0.1:8080/apply-template"},
		{"", "", ""},
		{"http://127.0.0.1:8080/v1", "qwen3.8", "http://127.0.0.1:8080/apply-template?model=qwen3.8"},
	}
	for _, tc := range cases {
		if got := llamaApplyTemplateURL(tc.base, tc.model); got != tc.want {
			t.Fatalf("llamaApplyTemplateURL(%q, %q)=%q, want %q", tc.base, tc.model, got, tc.want)
		}
	}
}

func TestParsePreserveReasoningCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		supported bool
		ok        bool
	}{
		{"cap-true", `{"chat_template_caps":{"supports_preserve_reasoning":true}}`, true, true},
		{"cap-false", `{"chat_template_caps":{"supports_preserve_reasoning":false}}`, false, true},
		{"missing-caps", `{}`, false, true},
		{"bad-json", `not-json`, false, false},
	}
	for _, tc := range cases {
		supported, ok := parsePreserveReasoningCap([]byte(tc.body))
		if supported != tc.supported || ok != tc.ok {
			t.Fatalf("%s: got (%v, %v), want (%v, %v)", tc.name, supported, ok, tc.supported, tc.ok)
		}
	}
}

// TestApplyChatCompletionExtrasFailedProbeNotCached verifies the cold-start
// fix: a probe that does not complete (404 here) must not pin "off" for the
// endpoint's lifetime — the next request re-probes and picks up the
// capability once the endpoint recovers.
func TestApplyChatCompletionExtrasFailedProbeNotCached(t *testing.T) {
	t.Parallel()

	var status atomic.Int32
	status.Store(http.StatusNotFound)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if s := status.Load(); s != http.StatusOK {
			http.Error(w, "nope", int(s))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]bool{"supports_preserve_reasoning": true},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/v1"}}}

	// First probe fails: no kwargs, and the negative result is NOT cached.
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params)
	assertNoChatTemplateKwargs(t, params)
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want 1", got)
	}

	// Endpoint recovers: the next request re-probes and sends the kwargs.
	status.Store(http.StatusOK)
	params2 := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params2)
	b, err := json.Marshal(params2)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	kwargs, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["preserve_reasoning"] != true {
		t.Fatalf("expected preserve_reasoning=true after recovery, got %s", b)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("/props hits = %d, want 2 (failed probe not cached)", got)
	}

	// The successful probe IS cached: no further hits.
	params3 := openai.ChatCompletionNewParams{Model: "test-model"}
	p.applyChatCompletionExtras(context.Background(), &params3)
	if got := hits.Load(); got != 2 {
		t.Fatalf("/props hits = %d, want 2 (success cached)", got)
	}
}

func TestProjectPromptCacheKeyStable(t *testing.T) {
	t.Parallel()
	a := ProjectPromptCacheKey("/tmp/project")
	b := ProjectPromptCacheKey("/tmp/project")
	if a == "" || a != b {
		t.Fatalf("unstable key: %q vs %q", a, b)
	}
	if ProjectPromptCacheKey("/tmp/other") == a {
		t.Fatal("different dirs should hash differently")
	}
}
