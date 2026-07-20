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
	var fullReasoning string
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

func TestIngestRawDeltaObject(t *testing.T) {
	t.Parallel()
	acc := newExtraFieldAccums()
	var fullReasoning string
	ingestRawDeltaObject(`{"reasoning_content":"step one"}`, acc, acc.thinkingEmitter(nil, &fullReasoning), nil)
	if got := acc.primaryDisplayText(); got != "step one" {
		t.Fatalf("got %q", got)
	}
	if fullReasoning != "step one" {
		t.Fatalf("fullReasoning = %q, want %q", fullReasoning, "step one")
	}
}

func TestDuplicateReasoningFieldsEmitOnce(t *testing.T) {
	t.Parallel()
	var thinking []string
	var fullReasoning string
	onThinking := func(s string) { thinking = append(thinking, s) }
	acc := newExtraFieldAccums()
	ingestRawDeltaObject(
		`{"reasoning_content":"Now I have a","reasoning":"Now I have a"}`,
		acc, acc.thinkingEmitter(onThinking, &fullReasoning), nil,
	)
	if len(thinking) != 1 || thinking[0] != "Now I have a" {
		t.Fatalf("thinking emissions = %#v, want single %q", thinking, "Now I have a")
	}
	if fullReasoning != "Now I have a" {
		t.Fatalf("fullReasoning = %q, want %q", fullReasoning, "Now I have a")
	}
}
