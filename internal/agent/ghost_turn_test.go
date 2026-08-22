package agent

import (
	"context"
	"os"
	"path/filepath"
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

// TestStreamProcessInputReportsFinishReason verifies the ghost-turn error
// carries the provider-reported finish_reason ("length" = output budget
// exhausted, "stop" = ended after reasoning-only chunks) so truncated turns
// are diagnosable without provider-side logs. Stubs that set no finish
// reason keep the original message (see TestStreamProcessInputRejectsGhostResult).
func TestStreamProcessInputReportsFinishReason(t *testing.T) {
	provider := &statsStubProvider{
		limit: 1000,
		streamResult: &llm.StreamResult{
			Reasoning:    "half a thought",
			FinishReason: "length",
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	_, err := a.StreamProcessInput(context.Background(), "investigate the flow", nil)
	if err == nil {
		t.Fatal("expected error for reasoning-only result")
	}
	if !strings.Contains(err.Error(), `finish_reason="length"`) {
		t.Fatalf("error should carry the finish reason: %v", err)
	}
	if len(a.Messages) != 1 || a.Messages[0].Role != "user" {
		t.Fatalf("ghost must not be appended; got %d messages: %+v", len(a.Messages), a.Messages)
	}
}

// ghostSequenceProvider serves a scripted sequence of stream results so the
// ghost-retry tests can observe the two rounds (ghost, then outcome).
type ghostSequenceProvider struct {
	statsStubProvider
	results []*llm.StreamResult
	calls   int
}

func (s *ghostSequenceProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	i := s.calls
	s.calls++
	if i < len(s.results) {
		return s.results[i], nil
	}
	return s.results[len(s.results)-1], nil
}

// TestStreamProcessInputRetriesGhostRoundOnce verifies the in-loop ghost
// recovery: a round that ended with reasoning only is retried automatically,
// the recovered answer is persisted as the turn's single assistant message,
// and no ghost turn leaks into the transcript between the user message and it.
func TestStreamProcessInputRetriesGhostRoundOnce(t *testing.T) {
	provider := &ghostSequenceProvider{
		results: []*llm.StreamResult{
			{Reasoning: "cut off mid-thought", FinishReason: "stop"},
			{Content: "recovered answer", Reasoning: "second attempt", FinishReason: "stop"},
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	out, err := a.StreamProcessInput(context.Background(), "investigate the flow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "recovered answer" {
		t.Fatalf("out=%q, want the retried round's content", out)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (ghost round + retry)", provider.calls)
	}
	if len(a.Messages) != 2 || a.Messages[1].Role != "assistant" || a.Messages[1].Content != "recovered answer" {
		t.Fatalf("transcript must hold exactly user+assistant; got %d messages: %+v", len(a.Messages), a.Messages)
	}
}

// TestStreamProcessInputGhostRetryBudgetExhausts verifies the retry is capped
// at one per CONSECUTIVE ghost sequence: a second back-to-back ghost round
// surfaces the finish-reason error instead of looping, and the transcript
// still holds nothing but the user message. (A successful round in between
// resets the counter — see
// TestStreamProcessInputGhostBudgetResetsAfterSuccessfulRound.) Both rounds use finish_reason="stop" — the stop-after-reasoning
// shape observed in practice — and the surfaced reason is the FINAL attempt's
// (rounds may report different causes; the guard only passes through what
// the failing round reported).
func TestStreamProcessInputGhostRetryBudgetExhausts(t *testing.T) {
	provider := &ghostSequenceProvider{
		results: []*llm.StreamResult{
			{Reasoning: "first truncated thought", FinishReason: "stop"},
			{Reasoning: "second truncated thought", FinishReason: "stop"},
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	_, err := a.StreamProcessInput(context.Background(), "investigate the flow", nil)
	if err == nil {
		t.Fatal("expected error after exhausting the ghost retry budget")
	}
	if !strings.Contains(err.Error(), `finish_reason="stop"`) {
		t.Fatalf("terminal error should carry the finish reason: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want exactly 2 (one retry, no loop)", provider.calls)
	}
	if len(a.Messages) != 1 || a.Messages[0].Role != "user" {
		t.Fatalf("ghost rounds must not append; got %d messages: %+v", len(a.Messages), a.Messages)
	}
}

// TestStreamProcessInputGhostBudgetResetsAfterSuccessfulRound verifies the
// ghost-retry budget counts CONSECUTIVE ghost rounds: a successful
// tool-call round between two ghosts resets the counter, so the second
// ghost still gets its own retry instead of failing the turn. Before the
// reset existed, the second ghost exhausted the per-turn budget and
// aborted a turn whose model had demonstrably recovered.
func TestStreamProcessInputGhostBudgetResetsAfterSuccessfulRound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &ghostSequenceProvider{
		results: []*llm.StreamResult{
			{Reasoning: "first truncated thought", FinishReason: "stop"},
			{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: map[string]any{"path": "note.txt"}}}},
			{Reasoning: "second truncated thought", FinishReason: "stop"},
			{Content: "final answer", FinishReason: "stop"},
		},
	}
	a := NewAgent(provider, &Executor{WorkingDir: dir}, nil)
	out, err := a.StreamProcessInput(context.Background(), "investigate the flow", nil)
	if err != nil {
		t.Fatalf("second ghost after a successful round must still be retried: %v", err)
	}
	if out != "final answer" {
		t.Fatalf("out=%q, want the second retry's content", out)
	}
	if provider.calls != 4 {
		t.Fatalf("provider calls = %d, want 4 (ghost, tool round, ghost, retry)", provider.calls)
	}
}
