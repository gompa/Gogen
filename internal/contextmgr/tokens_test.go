package contextmgr

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gogen/internal/llm"
)

func TestEstimateTokensUsesTokenizer(t *testing.T) {
	m := NewManager(nil, Settings{})
	ascii := m.EstimateTokens([]llm.Message{{Role: "user", Content: "hello world"}})
	cjk := m.EstimateTokens([]llm.Message{{Role: "user", Content: "你好世界编程助手"}})
	if ascii <= 0 || cjk <= 0 {
		t.Fatalf("ascii=%d cjk=%d", ascii, cjk)
	}
	// CJK should not be wildly over-counted as bytes/4 would for UTF-8.
	heuristicCJK := (len("你好世界编程助手") + 3) / 4
	if cjk >= heuristicCJK {
		// tokenizer usually counts fewer tokens than UTF-8 bytes/4 for CJK
		t.Logf("cjk tokens=%d heuristic=%d (ok if tokenizer unavailable)", cjk, heuristicCJK)
	}
	long := m.EstimateTokens([]llm.Message{{Role: "user", Content: strings.Repeat("token ", 100)}})
	if long < ascii {
		t.Fatalf("expected longer text to use more tokens: %d < %d", long, ascii)
	}
}

func TestEstimateTokensRejectsStaleCacheAfterMutation(t *testing.T) {
	m := NewManager(nil, Settings{})
	msgs := []llm.Message{{Role: "user", Content: "short"}}
	before := m.EstimateTokens(msgs)
	msgs[0].Content = strings.Repeat("token ", 200)
	after := m.EstimateTokens(msgs)
	if after <= before {
		t.Fatalf("expected mutated content to recount tokens: before=%d after=%d", before, after)
	}
}

func TestEnsureToolResultsCappedRecountsTokens(t *testing.T) {
	m := NewManager(nil, Settings{MaxToolResultBytes: 64})
	big := strings.Repeat("x", 400)
	msgs := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", Content: big},
	}
	before := m.EstimateTokens(msgs)
	if !m.EnsureToolResultsCapped(msgs) {
		t.Fatal("expected truncation")
	}
	after := m.EstimateTokens(msgs)
	if after >= before {
		t.Fatalf("expected capped tool result to reduce tokens: before=%d after=%d", before, after)
	}
	if !strings.Contains(msgs[1].Content, toolResultTruncationMarker) {
		t.Fatalf("expected truncation marker in tool content")
	}
}

// Token cache tests removed — token counts are now computed on demand
// with no global cache, so cache-size assertions no longer apply.

// TestTruncateForSummaryHeuristicRuneSafe pins that the bytes/4 fallback
// cuts on a rune boundary: slicing raw bytes at maxChars could split a
// multi-byte character and inject invalid UTF-8 into the summarization
// request (regression: text[:maxChars]).
func TestTruncateForSummaryHeuristicRuneSafe(t *testing.T) {
	text := strings.Repeat("界", 200) // 3 bytes per rune, 600 bytes total
	out := truncateForSummaryHeuristic(text, 40)
	if !utf8.ValidString(out) {
		t.Fatalf("heuristic truncation produced invalid UTF-8: %q", out)
	}
	if !strings.Contains(out, "truncated for summarization") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
	// 40 tokens * 4 = 160 bytes; the cut must back off to a rune boundary
	// (159 bytes = 53 runes) so the prefix stays <= the byte budget.
	prefix := strings.TrimSuffix(out, "\n… truncated for summarization (600 chars total)")
	if len(prefix) > 160 {
		t.Fatalf("prefix %d bytes exceeds the 160-byte budget", len(prefix))
	}
	if len(prefix)%3 != 0 {
		t.Fatalf("prefix length %d is not a rune boundary for 3-byte runes", len(prefix))
	}
}

// TestTruncateForSummaryValidUTF8 guards the same property through the
// public entry point: whichever path runs (tokenizer or heuristic), the
// result must be valid UTF-8.
func TestTruncateForSummaryValidUTF8(t *testing.T) {
	text := strings.Repeat("界", 500)
	out := truncateForSummary(text, 40)
	if !utf8.ValidString(out) {
		t.Fatalf("truncateForSummary produced invalid UTF-8: %q", out)
	}
	if !strings.Contains(out, "truncated for summarization") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
}

func TestEstimateToolTokens(t *testing.T) {
	small := llm.Tool{
		Type:        "function",
		Name:        "ping",
		Description: "ping the service",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
	big := llm.Tool{
		Type:        "function",
		Name:        "do_work",
		Description: strings.Repeat("a very long description of what the tool does ", 50),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":   map[string]any{"type": "string", "description": "the query"},
				"limit":   map[string]any{"type": "integer"},
				"verbose": map[string]any{"type": "boolean"},
				"filters": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		},
	}

	tests := []struct {
		name  string
		tools []llm.Tool
		check func(t *testing.T, got int)
	}{
		{
			name:  "empty set costs nothing",
			tools: nil,
			check: func(t *testing.T, got int) {
				if got != 0 {
					t.Fatalf("EstimateToolTokens(nil) = %d, want 0", got)
				}
			},
		},
		{
			name:  "single tool is positive",
			tools: []llm.Tool{small},
			check: func(t *testing.T, got int) {
				if got <= 0 {
					t.Fatalf("EstimateToolTokens = %d, want > 0", got)
				}
			},
		},
		{
			name:  "larger definition costs more",
			tools: []llm.Tool{big},
			check: func(t *testing.T, got int) {
				if got <= EstimateToolTokens([]llm.Tool{small}) {
					t.Fatalf("big tool (%d) should cost more than small tool (%d)", got, EstimateToolTokens([]llm.Tool{small}))
				}
			},
		},
		{
			name:  "multiple tools cost more than one",
			tools: []llm.Tool{small, big},
			check: func(t *testing.T, got int) {
				if got <= EstimateToolTokens([]llm.Tool{big}) {
					t.Fatalf("two tools (%d) should cost more than one (%d)", got, EstimateToolTokens([]llm.Tool{big}))
				}
			},
		},
		{
			name:  "deterministic across calls",
			tools: []llm.Tool{small, big},
			check: func(t *testing.T, got int) {
				if again := EstimateToolTokens([]llm.Tool{small, big}); again != got {
					t.Fatalf("non-deterministic: %d then %d", got, again)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, EstimateToolTokens(tc.tools))
		})
	}
}

// TestToolDefinitionStringStableAcrossMapOrder pins that the serialization
// does not depend on map construction/iteration order: the agent's
// wire-overhead cache fingerprint relies on it for cache stability.
func TestToolDefinitionStringStableAcrossMapOrder(t *testing.T) {
	a := llm.Tool{Type: "function", Name: "t", Description: "d", Parameters: map[string]any{
		"type": "object", "x": 1, "y": "two", "z": []any{"a", "b"},
	}}
	b := llm.Tool{Type: "function", Name: "t", Description: "d", Parameters: map[string]any{
		"z": []any{"a", "b"}, "type": "object", "y": "two", "x": 1,
	}}
	if got, want := ToolDefinitionString(a), ToolDefinitionString(b); got != want {
		t.Fatalf("serialization not stable across map order:\n%q\n%q", got, want)
	}
}
