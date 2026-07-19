package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestLatestIDUsesUpdatedNotMtime(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)

	if err := store.Save("older", agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "older"}},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := store.Save("newer", agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "newer"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Touch the older file so mtime is newer than "newer".
	olderPath := filepath.Join(dir, ".gogen", "sessions", "older.json")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(olderPath, future, future); err != nil {
		t.Fatal(err)
	}

	got, err := store.LatestID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "newer" {
		t.Fatalf("LatestID=%q want %q (should use Updated, not mtime)", got, "newer")
	}
}
