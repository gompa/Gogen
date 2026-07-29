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
