package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go"
)

func TestDecodeJSONFieldText(t *testing.T) {
	t.Parallel()
	if got := decodeJSONFieldText(`"hello"`); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := decodeJSONFieldText("plain"); got != "plain" {
		t.Fatalf("got %q", got)
	}
	if got := decodeJSONFieldText(`{"text":"nested"}`); got != "nested" {
		t.Fatalf("got %q", got)
	}
}

func TestExtraFieldAccumsSnapshot(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	var fullReasoning strings.Builder
	acc.addFromDelta(openai.ChatCompletionChunkChoiceDelta{}, nil, &fullReasoning)
	acc["reasoning_content"] = &strings.Builder{}
	acc["reasoning_content"].WriteString("thinking step")
	got := acc.snapshot()
	if got["reasoning_content"] != "thinking step" {
		t.Fatalf("snapshot = %#v", got)
	}
	if got := acc.primaryDisplayText(); got != "thinking step" {
		t.Fatalf("primaryDisplayText = %q", got)
	}
}

// deltaFromJSON builds a stream delta the way the SSE decoder does: unknown
// fields land in delta.JSON.ExtraFields (with status valid/null/invalid) and
// the raw JSON is retained.
func deltaFromJSON(t *testing.T, raw string) openai.ChatCompletionChunkChoiceDelta {
	t.Helper()
	var delta openai.ChatCompletionChunkChoiceDelta
	if err := json.Unmarshal([]byte(raw), &delta); err != nil {
		t.Fatalf("unmarshal delta: %v", err)
	}
	return delta
}

func TestAddFromDeltaReasoningField(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	var fullReasoning strings.Builder
	acc.addFromDelta(deltaFromJSON(t, `{"reasoning_content":"step one"}`), nil, &fullReasoning)
	if got := acc.primaryDisplayText(); got != "step one" {
		t.Fatalf("got %q", got)
	}
	if fullReasoning.String() != "step one" {
		t.Fatalf("fullReasoning = %q, want %q", fullReasoning.String(), "step one")
	}
}

func TestDuplicateReasoningFieldsEmitOnce(t *testing.T) {
	t.Parallel()
	var thinking []string
	var fullReasoning strings.Builder
	onThinking := func(s string) { thinking = append(thinking, s) }
	acc := newExtraFieldAccums()
	acc.addFromDelta(
		deltaFromJSON(t, `{"reasoning_content":"Now I have a","reasoning":"Now I have a"}`),
		onThinking, &fullReasoning,
	)
	if len(thinking) != 1 || thinking[0] != "Now I have a" {
		t.Fatalf("thinking emissions = %#v, want single %q", thinking, "Now I have a")
	}
	if fullReasoning.String() != "Now I have a" {
		t.Fatalf("fullReasoning = %q, want %q", fullReasoning.String(), "Now I have a")
	}
}

// TestAddFromDeltaStandardChunkIngestsNothing pins the hot-path contract: a
// standard OpenAI content chunk carries only known fields (role/content), so
// ExtraFields is empty and nothing may be ingested — the pre-fix RawJSON
// fallback wasted a full json.Unmarshal per chunk on exactly this shape.
func TestAddFromDeltaStandardChunkIngestsNothing(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	var fullReasoning strings.Builder
	acc.addFromDelta(deltaFromJSON(t, `{"role":"assistant","content":"answer"}`), nil, &fullReasoning)
	if len(acc) != 0 {
		t.Fatalf("standard chunk populated accumulators: %#v", acc)
	}
	if fullReasoning.Len() != 0 {
		t.Fatalf("standard chunk produced reasoning: %q", fullReasoning.String())
	}
}

// TestAddFromDeltaObjectTypedReasoning verifies a provider that sends
// reasoning as a JSON object (a type-mismatched extra field, Valid()==false)
// is still decoded. This was the only behavior the removed RawJSON fallback
// uniquely provided, so it is pinned here to prevent a silent regression.
func TestAddFromDeltaObjectTypedReasoning(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	var fullReasoning strings.Builder
	acc.addFromDelta(deltaFromJSON(t, `{"reasoning_content":{"content":"nested step"}}`), nil, &fullReasoning)
	if got := acc.primaryDisplayText(); got != "nested step" {
		t.Fatalf("got %q", got)
	}
	if fullReasoning.String() != "nested step" {
		t.Fatalf("fullReasoning = %q, want %q", fullReasoning.String(), "nested step")
	}
}

// TestAddFromDeltaNullExtraFieldIgnored verifies a present-but-null extra
// field (Valid()==false, Raw()=="null") is skipped, not ingested.
func TestAddFromDeltaNullExtraFieldIgnored(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	acc.addFromDelta(deltaFromJSON(t, `{"reasoning_content":null}`), nil, nil)
	if len(acc) != 0 {
		t.Fatalf("null extra field ingested: %#v", acc)
	}
}
