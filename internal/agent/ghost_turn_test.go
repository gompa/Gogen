package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestStreamProcessInputRejectsGhostResult verifies that a model result with
// no content, no refusal, and no tool calls — a stream truncated mid-reasoning
// (e.g. finish_reason="length" after consuming the output budget) — is
// surfaced as an error and never appended to the conversation. Previously it
// was persisted as a ghost assistant turn that rendered as an empty reply and
// later became a fork point.
func TestStreamProcessInputRejectsGhostResult(t *testing.T) {
	provider := &statsStubProvider{
		limit: 1000,
		streamResult: &llm.StreamResult{
			Reasoning: "OK let me now step back and put together the analysis. I've",
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	_, err := a.StreamProcessInput(context.Background(), "investigate the flow", nil)
	if err == nil {
		t.Fatal("expected error for reasoning-only result")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(a.Messages) != 1 || a.Messages[0].Role != "user" {
		t.Fatalf("ghost must not be appended; got %d messages: %+v", len(a.Messages), a.Messages)
	}
}

// TestStreamProcessInputAcceptsRefusalWithoutContent verifies refusals, which
// legitimately carry no content, still produce a normal persisted assistant
// turn (the guard must only reject turns with nothing at all).
func TestStreamProcessInputAcceptsRefusalWithoutContent(t *testing.T) {
	provider := &statsStubProvider{
		limit: 1000,
		streamResult: &llm.StreamResult{
			Refusal: "I can't help with that.",
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	out, err := a.StreamProcessInput(context.Background(), "do the bad thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "I can't help with that." {
		t.Fatalf("out=%q", out)
	}
	if len(a.Messages) != 2 || a.Messages[1].Role != "assistant" || a.Messages[1].Refusal != "I can't help with that." {
		t.Fatalf("refusal turn not persisted: %+v", a.Messages)
	}
}

// TestStreamProcessInputAcceptsNormalContent verifies the happy path is
// unaffected: a normal content result is appended and returned.
func TestStreamProcessInputAcceptsNormalContent(t *testing.T) {
	provider := &statsStubProvider{
		limit: 1000,
		streamResult: &llm.StreamResult{
			Content:   "here is the answer",
			Reasoning: "let me think",
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	out, err := a.StreamProcessInput(context.Background(), "a question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "here is the answer" {
		t.Fatalf("out=%q", out)
	}
	if len(a.Messages) != 2 || a.Messages[1].Content != "here is the answer" {
		t.Fatalf("assistant turn not persisted: %+v", a.Messages)
	}
}
