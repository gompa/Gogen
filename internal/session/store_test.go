package session

import (
	"encoding/json"
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

func TestSavePreservesCreatedOnCacheMiss(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	id := "created-preserve"
	if err := store.Save(id, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".gogen", "sessions", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var first file
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Created.IsZero() {
		t.Fatal("expected Created to be set")
	}

	// New store instance = empty createdCache (simulates process restart
	// without Load). Save must still keep the original Created.
	time.Sleep(5 * time.Millisecond)
	store2 := NewStore(true)
	if err := store2.Save(id, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "hi again"}},
	}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var second file
	if err := json.Unmarshal(data, &second); err != nil {
		t.Fatal(err)
	}
	if !second.Created.Equal(first.Created) {
		t.Fatalf("Created reset on cache miss: first=%v second=%v", first.Created, second.Created)
	}
}

// TestAppendMessagesUpdatesIndexMessageCount verifies that incremental delta
// saves keep the session index's MessageCount accurate, so List does not
// report a stale count between full snapshots. The delta file is cumulative
// (all messages since the last full snapshot), so each AppendMessages carries
// the agent's total message count.
func TestAppendMessagesUpdatesIndexMessageCount(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	id := "delta-count"
	base := []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	if err := store.Save(id, agent.SessionSnapshot{WorkingDir: dir, Messages: base}); err != nil {
		t.Fatal(err)
	}

	// First incremental save: one new message (total 3).
	if err := store.AppendMessages(id, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "q2"}},
	}, 3); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 3 {
		t.Fatalf("after first delta, want 3 messages, got %+v", sessions)
	}

	// Second incremental save: the delta is cumulative (both new messages,
	// matching how doPersist writes a.Messages[lastSavedMsgCount:]); total 4.
	if err := store.AppendMessages(id, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages: []llm.Message{
			{Role: "user", Content: "q2"},
			{Role: "assistant", Content: "a2"},
		},
	}, 4); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 4 {
		t.Fatalf("after second delta, want 4 messages, got %+v", sessions)
	}

	// A full save refreshes the count from the snapshot itself.
	full := append(append([]llm.Message{}, base...),
		llm.Message{Role: "user", Content: "q2"},
		llm.Message{Role: "assistant", Content: "a2"},
	)
	if err := store.Save(id, agent.SessionSnapshot{WorkingDir: dir, Messages: full}); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 4 {
		t.Fatalf("after full save, want 4 messages, got %+v", sessions)
	}
}

// TestAppendMessagesMissingIndexEntry verifies a delta save for a session
// absent from the index is a no-op for the index (no entry created), keeping
// the legacy fallback scan as the source of truth.
func TestAppendMessagesMissingIndexEntry(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	id := "no-index-entry"
	// Write a session file without touching the index (simulates a legacy
	// directory before the first full save indexed it).
	path := filepath.Join(dir, ".gogen", "sessions", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"id":"no-index-entry","messages":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(id, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "q2"}},
	}, 2); err != nil {
		t.Fatal(err)
	}
	// The index file must not be created by AppendMessages; List falls back
	// to the legacy scan.
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", "index.json")); !os.IsNotExist(err) {
		t.Fatalf("index should not be created by AppendMessages: %v", err)
	}
	sessions, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session from legacy scan, got %+v", sessions)
	}
}
