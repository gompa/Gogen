package agent

import (
	"context"
	"strings"
	"testing"
)

// TestTodoDoneIDSurvivesToolCall pins the end-to-end todo tool behavior for
// action=done/remove: the id argument must reach the handler and mark the
// right item. It also pins the failure modes — a numeric id works (float64
// as decoded from model JSON), a quoted numeric string is coerced (models
// do quote numbers), a non-numeric string surfaces the real type error
// instead of being masked as "missing required argument" (which made the id
// look dropped), and an absent id reports missing.
func TestTodoDoneIDSurvivesToolCall(t *testing.T) {
	newAgent := func() (*Agent, *TodoManager) {
		a := &Agent{Mode: ModeAct, Executor: &Executor{WorkingDir: t.TempDir()}}
		tm := NewTodoManager(t.TempDir())
		if _, err := tm.AddTodo("task one"); err != nil {
			t.Fatal(err)
		}
		if _, err := tm.AddTodo("task two"); err != nil {
			t.Fatal(err)
		}
		a.TodoManager = tm
		return a, tm
	}

	t.Run("numeric id marks done", func(t *testing.T) {
		a, tm := newAgent()
		out, err := a.executeTool(context.Background(), llmToolCall("todo", map[string]interface{}{
			"action": "done",
			"id":     float64(1), // JSON decode shape from a native tool call
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "#1") {
			t.Fatalf("expected #1 in output, got %q", out)
		}
		if got := tm.ListTodos(); !strings.Contains(got, "task one") || strings.Contains(got, "✅ #2") {
			t.Fatalf("wrong item marked done:\n%s", got)
		}
	})

	t.Run("int64 id removes", func(t *testing.T) {
		a, tm := newAgent()
		out, err := a.executeTool(context.Background(), llmToolCall("todo", map[string]interface{}{
			"action": "remove",
			"id":     int64(2),
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "#2") {
			t.Fatalf("expected #2 in output, got %q", out)
		}
		if got := tm.ListTodos(); strings.Contains(got, "task two") {
			t.Fatalf("task two should be removed:\n%s", got)
		}
	})

	t.Run("quoted numeric string id is coerced", func(t *testing.T) {
		a, tm := newAgent()
		out, err := a.executeTool(context.Background(), llmToolCall("todo", map[string]interface{}{
			"action": "done",
			"id":     "1", // models sometimes quote numbers
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "#1") {
			t.Fatalf("expected #1 in output, got %q", out)
		}
		if got := tm.ListTodos(); !strings.Contains(got, "task one") || strings.Contains(got, "✅ #2") {
			t.Fatalf("wrong item marked done:\n%s", got)
		}
	})

	t.Run("non-numeric string id reports type error not missing", func(t *testing.T) {
		a, _ := newAgent()
		_, err := a.executeTool(context.Background(), llmToolCall("todo", map[string]interface{}{
			"action": "done",
			"id":     "abc",
		}))
		if err == nil {
			t.Fatal("expected error for non-numeric string id")
		}
		if !strings.Contains(err.Error(), "must be an integer") {
			t.Fatalf("expected type error, got: %v", err)
		}
		if strings.Contains(err.Error(), "missing required argument") {
			t.Fatalf("non-numeric id must not be masked as missing: %v", err)
		}
	})

	t.Run("absent id reports missing", func(t *testing.T) {
		a, _ := newAgent()
		_, err := a.executeTool(context.Background(), llmToolCall("todo", map[string]interface{}{
			"action": "done",
		}))
		if err == nil {
			t.Fatal("expected error for absent id")
		}
		if !strings.Contains(err.Error(), `missing required argument "id"`) {
			t.Fatalf("expected missing-argument error, got: %v", err)
		}
	})
}
