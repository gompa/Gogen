package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openai/openai-go"
)

// TestApplyThinkingLevelWireValues pins the Gogen thinking level → OpenAI
// reasoning_effort mapping. The wire values are the widely-supported set
// (low/medium/high): minimal folds onto low and xhigh/max fold onto high, so
// requests stay compatible with the majority of reasoning models. "off"/empty
// omit the parameter entirely, and unknown values are rejected.
func TestApplyThinkingLevelWireValues(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":        "",    // no level configured → parameter omitted
		"off":     "",    // off → parameter omitted
		"minimal": "low", // extra level folded onto the common set
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "high", // extra level folded onto the common set
		"max":     "high", // extra level folded onto the common set
		"bogus":   "",     // unknown level → parameter omitted
	}
	for level, want := range cases {
		p := &OpenAIProvider{}
		if level != "" {
			p.SetThinkingLevel(level)
		}
		params := openai.ChatCompletionNewParams{Model: "test-model"}
		p.applyThinkingLevel(context.Background(), &params)

		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		got, _ := body["reasoning_effort"].(string)
		if got != want {
			t.Fatalf("level %q → reasoning_effort = %q, want %q (body %s)", level, got, want, b)
		}
	}
}
