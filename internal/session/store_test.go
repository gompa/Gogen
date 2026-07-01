package session

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	snap := agent.SessionSnapshot{
		WorkingDir: dir,
		Model:      "gpt-4o",
		Mode:       "plan",
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}
	id := "test-session"
	if err := store.Save(id, snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello" {
		t.Fatalf("messages=%+v", loaded.Messages)
	}
	if loaded.Mode != "plan" {
		t.Fatalf("mode=%q", loaded.Mode)
	}
}
