package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gogen/internal/llm"
)

// toolOutputStubProvider returns a single execute_command tool call on the
// first round, then a plain content turn, so StreamProcessInput exercises
// the full tool-execution loop with the live-output sink.
type toolOutputStubProvider struct {
	mu    sync.Mutex
	calls int
}

func (s *toolOutputStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *toolOutputStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return &llm.StreamResult{
			ToolCalls: []llm.ToolCall{{
				ID:   "call_1",
				Name: "execute_command",
				Args: map[string]any{"command": "echo hello"},
			}},
		}, nil
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *toolOutputStubProvider) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *toolOutputStubProvider) SetThinkingLevel(string)                          {}
func (s *toolOutputStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *toolOutputStubProvider) SetModel(string) error { return nil }
func (s *toolOutputStubProvider) ModelName() string     { return "test-model" }

// TestStreamProcessInputDeliversToolOutput verifies that shell tool output is
// streamed to the OnToolOutput handler with the tool call identity and the
// exact command, and that the tool result still arrives afterwards.
func TestStreamProcessInputDeliversToolOutput(t *testing.T) {
	provider := &toolOutputStubProvider{}
	a := NewAgent(provider, NewExecutor(t.TempDir()), nil)

	var mu sync.Mutex
	var outID, outName, outCmd string
	var chunks []string
	results := 0

	h := &llm.StreamHandlers{
		OnToolOutput: func(id, name, command, chunk string) {
			mu.Lock()
			defer mu.Unlock()
			if outID == "" {
				outID, outName, outCmd = id, name, command
			}
			chunks = append(chunks, chunk)
		},
		OnToolResult: func(id, name, result string, success bool) {
			mu.Lock()
			defer mu.Unlock()
			results++
			if !success {
				t.Errorf("expected success for %s, got result %q", name, result)
			}
		},
	}

	if _, err := a.StreamProcessInput(context.Background(), "run something", h); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if outID != "call_1" || outName != "execute_command" || outCmd != "echo hello" {
		t.Fatalf("unexpected sink identity: id=%q name=%q command=%q", outID, outName, outCmd)
	}
	if got := strings.Join(chunks, ""); got != "hello\n" {
		t.Fatalf("unexpected streamed chunks: %q", got)
	}
	if results != 1 {
		t.Fatalf("expected 1 tool result, got %d", results)
	}
}
