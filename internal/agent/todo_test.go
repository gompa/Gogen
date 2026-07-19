package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestTodoManagerSessionIsolation(t *testing.T) {
	dir := t.TempDir()
	store := &stubSessionStore{}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   dir,
		SessionStore: store,
		SessionID:    "session-a",
		TodoManager:  NewTodoManager(dir),
	}
	if _, err := a.TodoManager.AddTodo("from session a"); err != nil {
		t.Fatal(err)
	}
	a.persistTodos()

	snapA := store.sessions["session-a"]
	if snapA.Todos == nil || len(snapA.Todos.Items) != 1 {
		t.Fatalf("session-a todos = %#v", snapA.Todos)
	}

	_, handled, err := a.HandleSessionCommand(context.Background(), "/new", "session-b")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !a.TodoManager.Empty() {
		t.Fatalf("expected empty todos after /new, got %s", a.TodoManager.ListTodos())
	}
	if _, err := a.TodoManager.AddTodo("from session b"); err != nil {
		t.Fatal(err)
	}
	a.persistTodos()

	_, handled, err = a.HandleSessionCommand(context.Background(), "resume session-a", "")
	if err != nil || !handled {
		t.Fatalf("resume handled=%v err=%v", handled, err)
	}
	list := a.TodoManager.ListTodos()
	if !strings.Contains(list, "from session a") {
		t.Fatalf("missing session-a todo: %q", list)
	}
	if strings.Contains(list, "from session b") {
		t.Fatalf("session-b todo leaked: %q", list)
	}
}

func TestRestoreSessionReplacesTodos(t *testing.T) {
	a := &Agent{
		WorkingDir:  "/tmp/project",
		TodoManager: NewTodoManager("/tmp/project"),
	}
	_, _ = a.TodoManager.AddTodo("stale")
	a.RestoreSession(context.Background(), SessionSnapshot{
		WorkingDir: "/tmp/project",
		Todos: &TodoList{
			Items:  []TodoItem{{ID: 2, Text: "restored", Status: "pending"}},
			NextID: 3,
		},
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	got := a.TodoManager.ListTodos()
	if !strings.Contains(got, "restored") || strings.Contains(got, "stale") {
		t.Fatalf("todos=%q", got)
	}
}

func TestRestoreSessionClearsTodosWhenMissing(t *testing.T) {
	a := &Agent{
		WorkingDir:  "/tmp/project",
		TodoManager: NewTodoManager("/tmp/project"),
	}
	_, _ = a.TodoManager.AddTodo("leak")
	a.RestoreSession(context.Background(), SessionSnapshot{
		WorkingDir: "/tmp/project",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	})
	if !a.TodoManager.Empty() {
		t.Fatalf("expected empty todos, got %s", a.TodoManager.ListTodos())
	}
}

func TestImportLegacyFileOnce(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".gogen", "todos.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(TodoList{
		Items:  []TodoItem{{ID: 1, Text: "legacy task", Status: "pending"}},
		NextID: 2,
	})
	if err := os.WriteFile(legacy, data, 0o644); err != nil {
		t.Fatal(err)
	}

	tm := NewTodoManager(dir)
	if !tm.ImportLegacyFile() {
		t.Fatal("expected legacy import")
	}
	if tm.Empty() || !strings.Contains(tm.ListTodos(), "legacy task") {
		t.Fatalf("todos=%q", tm.ListTodos())
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be renamed away, err=%v", err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("expected migrated backup: %v", err)
	}

	tm2 := NewTodoManager(dir)
	if tm2.ImportLegacyFile() {
		t.Fatal("legacy should not import again")
	}
}
