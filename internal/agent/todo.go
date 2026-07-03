package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	todoFilePath = ".gogen/todos.json"
	maxTodos     = 50
)

// TodoItem represents a single task.
type TodoItem struct {
	ID        int       `json:"id"`
	Text      string    `json:"text"`
	Status    string    `json:"status"` // pending, done
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
}

// TodoList manages a persistent todo list.
type TodoList struct {
	Items  []TodoItem `json:"items"`
	NextID int        `json:"next_id"`
}

// TodoManager handles todo operations.
type TodoManager struct {
	workingDir string
	todos      *TodoList
}

// NewTodoManager creates or loads a todo manager.
func NewTodoManager(workingDir string) *TodoManager {
	todos, err := loadTodos(workingDir)
	if err != nil {
		todos = &TodoList{Items: []TodoItem{}, NextID: 1}
	}
	return &TodoManager{
		workingDir: workingDir,
		todos:      todos,
	}
}

func loadTodos(workingDir string) (*TodoList, error) {
	path := filepath.Join(workingDir, todoFilePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var todos TodoList
	if err := json.Unmarshal(data, &todos); err != nil {
		return nil, err
	}
	return &todos, nil
}

func (m *TodoManager) save() error {
	path := filepath.Join(m.workingDir, todoFilePath)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.todos, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o644)
}

// AddTodo adds a new todo item.
func (m *TodoManager) AddTodo(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("todo text is required")
	}
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
	if err := m.save(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Added todo #%d: %s", item.ID, text), nil
}

// DoneTodo marks a todo as completed.
func (m *TodoManager) DoneTodo(id int) (string, error) {
	for i, item := range m.todos.Items {
		if item.ID == id {
			if item.Status == "done" {
				return fmt.Sprintf("Todo #%d is already done: %s", id, item.Text), nil
			}
			m.todos.Items[i].Status = "done"
			m.todos.Items[i].DoneAt = time.Now().UTC()
			if err := m.save(); err != nil {
				return "", err
			}
			return fmt.Sprintf("Marked todo #%d as done: %s", id, item.Text), nil
		}
	}
	return "", fmt.Errorf("todo #%d not found", id)
}

// RemoveTodo removes a todo item entirely.
func (m *TodoManager) RemoveTodo(id int) (string, error) {
	for i, item := range m.todos.Items {
		if item.ID == id {
			m.todos.Items = append(m.todos.Items[:i], m.todos.Items[i+1:]...)
			if err := m.save(); err != nil {
				return "", err
			}
			return fmt.Sprintf("Removed todo #%d: %s", id, item.Text), nil
		}
	}
	return "", fmt.Errorf("todo #%d not found", id)
}

// ListTodos returns a formatted list of all todos.
func (m *TodoManager) ListTodos() string {
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
	if err := m.save(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Cleared %d completed todos", cleared), nil
}
