package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogen/internal/ioutil"
	"gogen/internal/session"
)

const (
	todoFilePath = ".gogen/todos.json"
	maxTodos     = 50
)

// TodoItem and TodoList are the persisted todo value types. Canonical
// definitions live in internal/session (the storage layer owns the
// on-disk format); these aliases keep existing references compiling.
type (
	TodoItem = session.TodoItem
	TodoList = session.TodoList
)

// TodoManager handles in-memory todo operations for the current session.
// Persistence is via SessionSnapshot (or a legacy file when sessions are disabled).
type TodoManager struct {
	// mu guards todos and workingDir. Mutations happen on the turn goroutine
	// (todo tools run inside turns), but snapshots also run on flush paths
	// that hold no turn lock (ShutdownSessions after a drain timeout,
	// sessionDelete with a stuck turn, doPersist) — a plain field access
	// raced those readers (data race under -race).
	mu         sync.RWMutex
	workingDir string
	todos      *TodoList
}

// NewTodoManager creates an empty todo manager for workingDir.
func NewTodoManager(workingDir string) *TodoManager {
	return &TodoManager{
		workingDir: workingDir,
		todos:      &TodoList{Items: []TodoItem{}, NextID: 1},
	}
}

// Snapshot returns a deep copy of the current todo list for session persistence.
func (m *TodoManager) Snapshot() *TodoList {
	if m == nil {
		return &TodoList{Items: []TodoItem{}, NextID: 1}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.todos == nil {
		return &TodoList{Items: []TodoItem{}, NextID: 1}
	}
	out := &TodoList{
		Items:  append([]TodoItem(nil), m.todos.Items...),
		NextID: m.todos.NextID,
	}
	if out.NextID < 1 {
		out.NextID = 1
	}
	return out
}

// Replace replaces the in-memory todo list with a copy of list.
func (m *TodoManager) Replace(list *TodoList) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if list == nil {
		m.todos = &TodoList{Items: []TodoItem{}, NextID: 1}
		return
	}
	next := list.NextID
	if next < 1 {
		next = 1
	}
	m.todos = &TodoList{
		Items:  append([]TodoItem(nil), list.Items...),
		NextID: next,
	}
}

// Clear removes all todos from the current session.
func (m *TodoManager) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.todos = &TodoList{Items: []TodoItem{}, NextID: 1}
}

// Empty reports whether there are no todo items.
func (m *TodoManager) Empty() bool {
	if m == nil {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.todos == nil || len(m.todos.Items) == 0
}

// SetWorkingDir updates the directory used for legacy file fallback.
func (m *TodoManager) SetWorkingDir(dir string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workingDir = dir
}

// ImportLegacyFile loads `.gogen/todos.json` once into this manager and renames
// the file so it is not re-imported on later startups.
func (m *TodoManager) ImportLegacyFile() bool {
	if m == nil || m.workingDir == "" || !m.Empty() {
		return false
	}
	path := filepath.Join(m.workingDir, todoFilePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var todos TodoList
	if err := json.Unmarshal(data, &todos); err != nil {
		return false
	}
	if len(todos.Items) == 0 {
		_ = os.Remove(path)
		return false
	}
	bak := path + ".migrated"
	if err := os.Rename(path, bak); err != nil {
		// Still imported into memory; best-effort remove so we do not keep
		// re-reading a stuck legacy file on every start.
		_ = os.Remove(path)
	}
	m.Replace(&todos)
	return true
}

func (m *TodoManager) saveLegacy() error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.workingDir == "" {
		return nil
	}
	path := filepath.Join(m.workingDir, todoFilePath)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.todos, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFileAtomic(path, data, defaultFilePerm)
}

// AddTodo adds a new todo item.
func (m *TodoManager) AddTodo(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("todo text is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.todos.Items) >= maxTodos {
		return "", fmt.Errorf("too many todos (max %d); complete or remove some first", maxTodos)
	}
	item := TodoItem{
		ID:        m.todos.NextID,
		Text:      text,
		Status:    "pending",
		CreatedAt: time.Now().UTC(),
	}
	m.todos.NextID++
	m.todos.Items = append(m.todos.Items, item)
	return fmt.Sprintf("Added todo #%d: %s", item.ID, text), nil
}

// DoneTodo marks a todo as completed.
func (m *TodoManager) DoneTodo(id int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, item := range m.todos.Items {
		if item.ID == id {
			if item.Status == "done" {
				return fmt.Sprintf("Todo #%d is already done: %s", id, item.Text), nil
			}
			m.todos.Items[i].Status = "done"
			m.todos.Items[i].DoneAt = time.Now().UTC()
			return fmt.Sprintf("Marked todo #%d as done: %s", id, item.Text), nil
		}
	}
	return "", fmt.Errorf("todo #%d not found", id)
}

// RemoveTodo removes a todo item entirely.
func (m *TodoManager) RemoveTodo(id int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, item := range m.todos.Items {
		if item.ID == id {
			m.todos.Items = append(m.todos.Items[:i], m.todos.Items[i+1:]...)
			return fmt.Sprintf("Removed todo #%d: %s", id, item.Text), nil
		}
	}
	return "", fmt.Errorf("todo #%d not found", id)
}

// ListTodos returns a formatted list of all todos.
func (m *TodoManager) ListTodos() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.todos.Items) == 0 {
		return "No todos"
	}
	var b strings.Builder
	pending := 0
	done := 0
	for _, item := range m.todos.Items {
		status := "⏳"
		if item.Status == "done" {
			status = "✅"
			done++
		} else {
			pending++
		}
		fmt.Fprintf(&b, "%s #%d: %s\n", status, item.ID, item.Text)
	}
	b.WriteString(fmt.Sprintf("\n%d pending, %d done", pending, done))
	return b.String()
}

// ClearDoneTodos removes all completed todos.
func (m *TodoManager) ClearDoneTodos() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	remaining := make([]TodoItem, 0, len(m.todos.Items))
	cleared := 0
	for _, item := range m.todos.Items {
		if item.Status == "done" {
			cleared++
		} else {
			remaining = append(remaining, item)
		}
	}
	if cleared == 0 {
		return "No completed todos to clear", nil
	}
	m.todos.Items = remaining
	return fmt.Sprintf("Cleared %d completed todos", cleared), nil
}

// todo ensures the TodoManager is initialized and returns it.
func (a *Agent) todo() (*TodoManager, error) {
	if a.TodoManager == nil {
		return nil, fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager, nil
}
func todoSnapshot(m *TodoManager) *TodoList {
	if m == nil {
		return nil
	}
	return m.Snapshot()
}

// ImportLegacyTodos adopts a project-level `.gogen/todos.json` into the current
// session once, then persists the session so the todos become session-scoped.
func (a *Agent) ImportLegacyTodos() bool {
	if a.TodoManager == nil || !a.TodoManager.ImportLegacyFile() {
		return false
	}
	a.persistTodos()
	return true
}

// persistTodos writes todo changes: with the session when persistence is on,
// otherwise to the legacy project-level todos file.
func (a *Agent) persistTodos() {
	if a.SessionStore != nil && a.SessionID != "" {
		a.FlushSession()
		return
	}
	if a.TodoManager != nil {
		if err := a.TodoManager.saveLegacy(); err != nil {
			log.Printf("todo save failed: %v", err)
		}
	}
}
