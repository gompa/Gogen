package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/modelinfo"

	"github.com/openai/openai-go"
)

// effortRegistry writes a models.dev-style registry (opencode provider) with
// the given model → effort setup to a temp disk cache and returns a resolver
// that loads it synchronously.
//
//	efforts: accepted effort values for a reasoning_options effort entry
//	nil:     toggle-only model (no effort control)
func effortRegistry(t *testing.T, models map[string][]string) *modelinfo.Resolver {
	t.Helper()
	reg := map[string]any{
		"opencode": map[string]any{
			"id":     "opencode",
			"api":    "https://opencode.ai/zen/v1",
			"models": map[string]any{},
		},
	}
	modelsMap := reg["opencode"].(map[string]any)["models"].(map[string]any)
	for id, efforts := range models {
		entry := map[string]any{
			"id":    id,
			"limit": map[string]int{"context": 200000},
		}
		if efforts == nil {
			entry["reasoning_options"] = []map[string]any{
				{"type": "toggle"},
			}
		} else {
			entry["reasoning_options"] = []map[string]any{
				{"type": "effort", "values": efforts},
			}
		}
		modelsMap[id] = entry
	}
	dir := t.TempDir()
	cache := filepath.Join(dir, "models.json")
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return modelinfo.NewResolver(cache)
}

func thinkingEffortFromBody(t *testing.T, params openai.ChatCompletionNewParams) string {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	got, _ := body["reasoning_effort"].(string)
	return got
}

// TestApplyThinkingLevelStrictLiteral pins the verbatim, no-translation wire
// contract:
//
//   - known model (models.dev registry): the stored level is sent verbatim iff
//     it is in the model's accepted set; otherwise omitted (policy B).
//   - toggle-only model (no effort control): any level is omitted.
//   - unknown/self-hosted model: DefaultReasoningEfforts (low/medium/high)
//     applies; legacy values (minimal/xhigh/max) are NOT folded (option 2).
//   - "" / "off": parameter always omitted.
func TestApplyThinkingLevelStrictLiteral(t *testing.T) {
	t.Parallel()
	resolver := effortRegistry(t, map[string][]string{
		"glm-5.2": {"high", "max"},
		"o3-mini": {"low", "medium", "high"},
		"glm-5":   nil, // toggle-only
	})
	known := &OpenAIProvider{
		baseURL:   "https://opencode.ai/zen/v1/",
		modelInfo: resolver,
	}
	unknown := &OpenAIProvider{
		baseURL:   "http://127.0.0.1:1/v1", // no registry match → fallback
		modelInfo: resolver,
	}

	cases := []struct {
		name   string
		p      *OpenAIProvider
		model  string
		level  string
		effort string // expected reasoning_effort on the wire; "" = omitted
	}{
		// Known model accepting {high, max}: verbatim, no translation.
		{name: "known-high", p: known, model: "glm-5.2", level: "high", effort: "high"},
		{name: "known-max", p: known, model: "glm-5.2", level: "max", effort: "max"},
		{name: "known-low-not-accepted", p: known, model: "glm-5.2", level: "low", effort: ""},
		{name: "known-medium-not-accepted", p: known, model: "glm-5.2", level: "medium", effort: ""},
		{name: "known-none-not-accepted", p: known, model: "glm-5.2", level: "none", effort: ""},

		// Known model accepting {low, medium, high}.
		{name: "known-o3-medium", p: known, model: "o3-mini", level: "medium", effort: "medium"},
		{name: "known-o3-none-not-accepted", p: known, model: "o3-mini", level: "none", effort: ""},

		// Toggle-only model: no effort control → always omit.
		{name: "toggle-only-high", p: known, model: "glm-5", level: "high", effort: ""},

		// Unknown model: fallback DefaultReasoningEfforts {low, medium, high}.
		{name: "unknown-low", p: unknown, model: "selfhosted", level: "low", effort: "low"},
		{name: "unknown-medium", p: unknown, model: "selfhosted", level: "medium", effort: "medium"},
		{name: "unknown-high", p: unknown, model: "selfhosted", level: "high", effort: "high"},
		// Legacy values are not folded (option 2): max/xhigh/minimal ∉ fallback.
		{name: "unknown-legacy-max", p: unknown, model: "selfhosted", level: "max", effort: ""},
		{name: "unknown-legacy-xhigh", p: unknown, model: "selfhosted", level: "xhigh", effort: ""},
		{name: "unknown-legacy-minimal", p: unknown, model: "selfhosted", level: "minimal", effort: ""},

		// Off / empty → always omitted.
		{name: "off", p: known, model: "glm-5.2", level: "off", effort: ""},
		{name: "empty", p: known, model: "glm-5.2", level: "", effort: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.SetModel(tc.model); err != nil {
				t.Fatal(err)
			}
			if tc.level != "" {
				tc.p.SetThinkingLevel(tc.level)
			}
			params := openai.ChatCompletionNewParams{Model: tc.model}
			tc.p.applyThinkingLevel(context.Background(), &params)
			if got := thinkingEffortFromBody(t, params); got != tc.effort {
				t.Fatalf("level %q → reasoning_effort = %q, want %q", tc.level, got, tc.effort)
			}
		})
	}
}
