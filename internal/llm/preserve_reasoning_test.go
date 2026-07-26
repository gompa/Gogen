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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": map[string]bool{
				"supports_preserve_reasoning": true,
			},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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

	p := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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

	p := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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
	p2 := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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

	p := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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

	p := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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

	p := &OpenAIProvider{baseURL: srv.URL + "/v1"}
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
		in, want string
	}{
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/props"},
		{"http://127.0.0.1:8080/v1/", "http://127.0.0.1:8080/props"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/props"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := llamaPropsURL(tc.in); got != tc.want {
			t.Fatalf("llamaPropsURL(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParsePreserveReasoningCap(t *testing.T) {
	t.Parallel()
	if !parsePreserveReasoningCap([]byte(`{"chat_template_caps":{"supports_preserve_reasoning":true}}`)) {
		t.Fatal("expected true")
	}
	if parsePreserveReasoningCap([]byte(`{"chat_template_caps":{"supports_preserve_reasoning":false}}`)) {
		t.Fatal("expected false")
	}
	if parsePreserveReasoningCap([]byte(`{}`)) {
		t.Fatal("expected false for missing caps")
	}
	if parsePreserveReasoningCap([]byte(`not-json`)) {
		t.Fatal("expected false for bad json")
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
