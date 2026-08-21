package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestAppendArchiveJSONL pins the Phase 5 sidecar format: an append-only
// JSONL file next to the session file, one self-contained entry per line.
func TestAppendArchiveJSONL(t *testing.T) {
	wd := t.TempDir()
	s := NewStore(true)
	first := agent.ArchiveEntry{
		TS:      time.Now().UTC().Truncate(time.Second),
		Kind:    "condensed_message",
		Index:   0,
		Role:    "user",
		Tokens:  3000,
		Content: "the original message",
	}
	if err := s.AppendArchive(wd, "sess1", first); err != nil {
		t.Fatalf("AppendArchive: %v", err)
	}
	second := agent.ArchiveEntry{Kind: "condensed_message", Index: 2, Role: "tool", Content: "second"}
	if err := s.AppendArchive(wd, "sess1", second); err != nil {
		t.Fatalf("AppendArchive (second): %v", err)
	}

	path := filepath.Join(wd, ".gogen", "sessions", "sess1.archive.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("sidecar lines = %d, want 2", len(lines))
	}
	var got agent.ArchiveEntry
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("first line is not valid JSON: %v", err)
	}
	if got.Kind != "condensed_message" || got.Role != "user" || got.Index != 0 ||
		got.Tokens != 3000 || got.Content != "the original message" {
		t.Fatalf("first entry = %+v, want the original message", got)
	}
	var got2 agent.ArchiveEntry
	if err := json.Unmarshal([]byte(lines[1]), &got2); err != nil {
		t.Fatalf("second line is not valid JSON: %v", err)
	}
	if got2.Role != "tool" || got2.Content != "second" {
		t.Fatalf("second entry = %+v", got2)
	}
}

// TestDeleteRemovesArchive pins the cleanup: deleting a session removes its
// archive sidecar too (the shadowed content belongs to the session).
func TestDeleteRemovesArchive(t *testing.T) {
	wd := t.TempDir()
	s := NewStore(true)
	if err := s.AppendArchive(wd, "sess1", agent.ArchiveEntry{Kind: "condensed_message", Content: "x"}); err != nil {
		t.Fatalf("AppendArchive: %v", err)
	}
	// The session file itself does not exist (the sidecar is created
	// independently): Delete must still clean the sidecar up.
	if err := s.Delete(wd, "sess1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	path := filepath.Join(wd, ".gogen", "sessions", "sess1.archive.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("archive sidecar survived Delete (stat err = %v)", err)
	}
}

// TestAppendArchiveDisabledStore pins the disabled-store no-op: persistence
// off means no sidecar is written (the agent reports the archive failure in
// the in-band notice).
func TestAppendArchiveDisabledStore(t *testing.T) {
	s := NewStore(false)
	if err := s.AppendArchive(t.TempDir(), "sess1", agent.ArchiveEntry{Kind: "condensed_message"}); err == nil {
		t.Fatal("AppendArchive on a disabled store: want an error")
	}
}
