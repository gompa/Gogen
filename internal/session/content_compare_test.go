package session

import (
	"testing"
	"time"

	"gogen/internal/llm"
)

// TestSameMessageContent pins the persisted-content contract used by
// deltaPrefixMatches: two messages are equal only when every persisted field
// round-trips identically, including the provider-reported Model and attached
// images (both were previously missing from the comparison, so a snapshot and
// a delta that differed only in those fields were wrongly treated as the same
// message during the two-writer race tail-merge).
func TestSameMessageContent(t *testing.T) {
	created := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	base := llm.Message{
		Role:      "assistant",
		Content:   "answer",
		Reasoning: "thinking step",
		Refusal:   "no refusal",
		Model:     "glm-4.6",
		CreatedAt: created,
		Images:    []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA", Detail: "high"}},
		ToolCalls: []llm.ToolCall{{
			ID:   "c1",
			Name: "read_file",
			Args: map[string]any{"path": "main.go"},
		}},
	}
	if !sameMessageContent(base, base) {
		t.Fatal("identical messages must be equal")
	}

	cases := []struct {
		name string
		mut  func(*llm.Message)
	}{
		{"role", func(m *llm.Message) { m.Role = "user" }},
		{"content", func(m *llm.Message) { m.Content = "other" }},
		{"reasoning", func(m *llm.Message) { m.Reasoning = "other thinking" }},
		{"refusal", func(m *llm.Message) { m.Refusal = "other refusal" }},
		{"toolCallID", func(m *llm.Message) { m.ToolCallID = "t1" }},
		{"model", func(m *llm.Message) { m.Model = "other-model" }},
		{"createdAt", func(m *llm.Message) { m.CreatedAt = created.Add(time.Second) }},
		{"imagesDataURL", func(m *llm.Message) {
			m.Images = []llm.ImageInput{{DataURL: "data:image/png;base64,BBBB", Detail: "high"}}
		}},
		{"imagesDetail", func(m *llm.Message) {
			m.Images = []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA", Detail: "low"}}
		}},
		{"imagesLen", func(m *llm.Message) { m.Images = nil }},
		{"toolCallID2", func(m *llm.Message) { m.ToolCalls[0].ID = "c2" }},
		{"toolCallName", func(m *llm.Message) { m.ToolCalls[0].Name = "search_code" }},
		{"toolCallArgs", func(m *llm.Message) { m.ToolCalls[0].Args = map[string]any{"path": "other.go"} }},
		{"toolCallArgsStr", func(m *llm.Message) { m.ToolCalls[0].ArgsStr = `{"path":"other.go"}` }},
		{"toolCallsLen", func(m *llm.Message) {
			m.ToolCalls = append(m.ToolCalls, llm.ToolCall{ID: "c2", Name: "list_files"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cloneForCompare(base)
			tc.mut(&got)
			if sameMessageContent(base, got) {
				t.Fatalf("messages differing in %s must not be equal", tc.name)
			}
		})
	}
}

// cloneForCompare deep-copies the slice fields a test mutator touches so a
// mutation cannot leak into the shared base message via a copied slice header.
func cloneForCompare(m llm.Message) llm.Message {
	out := m
	if len(m.Images) > 0 {
		out.Images = append([]llm.ImageInput(nil), m.Images...)
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = make([]llm.ToolCall, len(m.ToolCalls))
		for i := range m.ToolCalls {
			out.ToolCalls[i] = m.ToolCalls[i]
			if m.ToolCalls[i].Args != nil {
				out.ToolCalls[i].Args = make(map[string]any, len(m.ToolCalls[i].Args))
				for k, v := range m.ToolCalls[i].Args {
					out.ToolCalls[i].Args[k] = v
				}
			}
		}
	}
	return out
}

// TestDeltaPrefixMatchesRejectsModelOnlyDifference verifies the race-merge
// decision treats a Model-only divergence as a mismatch: the snapshot did not
// absorb the delta's prefix (they are different messages), so the conservative
// drop-delta path must win.
func TestDeltaPrefixMatchesRejectsModelOnlyDifference(t *testing.T) {
	snap := []llm.Message{{Role: "assistant", Content: "hi", Model: "glm-4.6"}}
	delta := []llm.Message{{Role: "assistant", Content: "hi", Model: "other-model"}}
	if deltaPrefixMatches(snap, delta) {
		t.Fatal("deltaPrefixMatches must reject a Model-only difference")
	}
	delta[0].Model = "glm-4.6"
	if !deltaPrefixMatches(snap, delta) {
		t.Fatal("deltaPrefixMatches must accept identical persisted content")
	}
}
