package llm

import (
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
	acc.addFromDelta(openai.ChatCompletionChunkChoiceDelta{}, nil)
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

func TestIngestRawDeltaObject(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	ingestRawDeltaObject(`{"reasoning_content":"step one"}`, acc, nil, nil)
	if got := acc.primaryDisplayText(); got != "step one" {
		t.Fatalf("got %q", got)
	}
}
