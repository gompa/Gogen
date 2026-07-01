package llm

import "testing"

func TestParseContextLimitFromJSON(t *testing.T) {
	raw := `{"id":"gpt-4o","context_length":128000}`
	if got := parseContextLimitFromJSON(raw); got != 128000 {
		t.Fatalf("expected 128000, got %d", got)
	}

	nctx := `{"id":"some-model","n_ctx":200192}`
	if got := parseContextLimitFromJSON(nctx); got != 200192 {
		t.Fatalf("expected 200192 from n_ctx, got %d", got)
	}

	llamacpp := `{"id":"Qwen3.6-27B-UD-Q4_K_XL.gguf","object":"model","owned_by":"llamacpp","meta":{"n_ctx":200192,"n_ctx_train":262144}}`
	if got := parseContextLimitFromJSON(llamacpp); got != 200192 {
		t.Fatalf("expected 200192 from meta.n_ctx, got %d", got)
	}
}

func TestInferContextLimitFromModelName(t *testing.T) {
	cases := map[string]int{
		"gpt-4o":            128000,
		"gpt-4o-mini":       128000,
		"gpt-4-32k":         32768,
		"gpt-3.5-turbo-16k": 16385,
		"deepseek-v4-flash": 1000000,
		"deepseek-v4-pro":   1000000,
		"deepseek-chat":     128000,
		"deepseek-reasoner": 128000,
		"deepseek-coder":    128000,
		"deepseek-unknown":  128000,
		"gemini-2.5-pro":    1048576,
		"gemini-2.5-flash":  1048576,
		"gemini-2.0-flash":  1048576,
		"gemini-1.5-pro":    2097152,
		"gemini-1.5-flash":  1048576,
		"gemini-pro":        32768,
		"claude-3.5-sonnet": 200000,
		"claude-3-5-sonnet": 200000,
		"claude-3-opus":     200000,
		"claude-sonnet-4":   200000,
		"o1":                200000,
		"o3-mini":           200000,
	}
	for model, want := range cases {
		if got := inferContextLimitFromModelName(model); got != want {
			t.Fatalf("%s: expected %d, got %d", model, want, got)
		}
	}
}
