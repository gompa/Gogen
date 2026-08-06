package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// fakeMCPRegistry is a minimal MCPToolRegistry for eligibility tests.
type fakeMCPRegistry struct {
	names map[string]struct{}
}

func (f *fakeMCPRegistry) Definitions() []llm.Tool        { return nil }
func (f *fakeMCPRegistry) ToolNames() map[string]struct{} { return f.names }
func (f *fakeMCPRegistry) CallTool(context.Context, string, map[string]interface{}) (string, error) {
	return "", nil
}

// TestToolCallsParallelEligible verifies the batch classifier: only batches
// of 2+ builtin read-only tools are eligible, and mutating or MCP-shadowed
// names force the sequential path.
func TestToolCallsParallelEligible(t *testing.T) {
	a := &Agent{}
	readOnly := []llm.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c2", Name: "search_code"},
	}
	if !a.toolCallsParallelEligible(readOnly) {
		t.Fatal("two read-only calls should be eligible")
	}
	if a.toolCallsParallelEligible(readOnly[:1]) {
		t.Fatal("a single call should not be parallel")
	}
	mixed := []llm.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c3", Name: "write_file"},
	}
	if a.toolCallsParallelEligible(mixed) {
		t.Fatal("a batch with a mutating tool must run sequentially")
	}
	// MCP-shadowed names are excluded even when every name is a builtin.
	a.MCPRegistry = &fakeMCPRegistry{names: map[string]struct{}{"read_file": {}}}
	if a.toolCallsParallelEligible(readOnly) {
		t.Fatal("an MCP-shadowed name must force the sequential path")
	}
}

// TestExecuteToolCallsParallelRunsReadOnlyToolsConcurrently verifies that a
// batch of read-only tool calls is executed concurrently (observed overlap),
// that every tool_call gets a matching tool result, and that results are
// appended in the model's call order.
func TestExecuteToolCallsParallelRunsReadOnlyToolsConcurrently(t *testing.T) {
	dir := t.TempDir()
	files := []string{"a.txt", "b.txt", "c.txt"}
	for i, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content-"+string(rune('a'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prov := llm.NewMockProvider()
	prov.StreamResults = []*llm.StreamResult{
		{
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "read_file", Args: map[string]interface{}{"path": "a.txt"}},
				{ID: "c2", Name: "read_file", Args: map[string]interface{}{"path": "b.txt"}},
				{ID: "c3", Name: "read_file", Args: map[string]interface{}{"path": "c.txt"}},
			},
		},
		{Content: "done"},
	}
	exec := NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	// Wrap read_file with an overlap detector: record the max number of
	// concurrent executions. A sequential runner would never exceed 1.
	var mu sync.Mutex
	active, maxActive := 0, 0
	builtin := BuiltinToolHandlers()
	orig := builtin["read_file"]
	builtin["read_file"] = func(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		time.Sleep(30 * time.Millisecond)
		return orig(ctx, a, args)
	}
	a.SetToolHandlers(builtin)

	if _, err := a.StreamProcessInput(context.Background(), "read three files", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	if maxActive < 2 {
		t.Fatalf("expected concurrent read_file executions (maxActive=%d), tools did not run in parallel", maxActive)
	}

	// Every tool_call must have a matching tool result, in call order.
	var toolOrder []string
	results := map[string]string{}
	for _, m := range a.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				toolOrder = append(toolOrder, tc.ID)
			}
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m.Content
		}
	}
	want := []string{"c1", "c2", "c3"}
	if len(toolOrder) != len(want) {
		t.Fatalf("tool call order = %v, want %v", toolOrder, want)
	}
	for i := range want {
		if toolOrder[i] != want[i] {
			t.Fatalf("tool call order = %v, want %v", toolOrder, want)
		}
	}
	for i, id := range want {
		got := results[id]
		if got == "" {
			t.Fatalf("tool call %s has no result", id)
		}
		if !strings.Contains(got, "content-"+string(rune('a'+i))) {
			t.Fatalf("tool call %s result does not contain file %d content: %q", id, i, got)
		}
	}
}

// TestToolCallsParallelCancelStillClosesEveryToolCall verifies the cancel
// path: when the turn context is cancelled mid-parallel-execution, every
// tool_call still receives a matching tool result so the next turn's
// tool-call/result protocol stays valid.
func TestToolCallsParallelCancelStillClosesEveryToolCall(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llm.NewMockProvider()
	prov.StreamResults = []*llm.StreamResult{
		{
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "read_file", Args: map[string]interface{}{"path": "a.txt"}},
				{ID: "c2", Name: "read_file", Args: map[string]interface{}{"path": "a.txt"}},
			},
		},
		{Content: "done"},
	}
	exec := NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	builtin := BuiltinToolHandlers()
	orig := builtin["read_file"]
	builtin["read_file"] = func(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
		// Block long enough for the cancel to land mid-flight.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
		return orig(ctx, a, args)
	}
	a.SetToolHandlers(builtin)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.StreamProcessInput(ctx, "read", nil)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// The assistant message's tool calls must all have results.
	found := map[string]bool{}
	for _, m := range a.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			found[m.ToolCallID] = true
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if !found[id] {
			t.Fatalf("tool call %s missing a result after cancel", id)
		}
	}
}
