package session

import (
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

func TestListIncludesLabelAndCount(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	snap := agent.SessionSnapshot{
		WorkingDir: dir,
		Model:      "gpt-4o",
		Mode:       "act",
		Messages: []llm.Message{
			{Role: "user", Content: "implement session commands with labels"},
			{Role: "assistant", Content: "ok"},
		},
	}
	if err := store.Save("sess-1", snap); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions", len(list))
	}
	if list[0].MessageCount != 2 {
		t.Fatalf("count=%d", list[0].MessageCount)
	}
	if !strings.Contains(list[0].Label, "implement session") {
		t.Fatalf("label=%q", list[0].Label)
	}
}

// TestListKeepsRenamedLabel verifies the reported bug: a session renamed via
// RenameSession / the session_rename tool keeps its custom label in the
// listing. Pre-fix, sessionLabel always regenerated the index label from the
// messages, so the sidebar showed the rename while the session was open (the
// live pane label) but fell back to the first user message as soon as the
// pane was closed (the saved-session row reads the index entry).
func TestListKeepsRenamedLabel(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	snap := agent.SessionSnapshot{
		WorkingDir: dir,
		Messages: []llm.Message{
			{Role: "user", Content: "implement session commands with labels"},
			{Role: "assistant", Content: "ok"},
		},
		// What RenameSession persists: a custom label distinct from the
		// first user message.
		Label: "My Custom Title",
	}
	if err := store.Save("sess-1", snap); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions", len(list))
	}
	if list[0].Label != "My Custom Title" {
		t.Fatalf("label=%q, want the renamed title (not the first user message)", list[0].Label)
	}
}

// TestSavePersistsRenamedEmptySession pins the label exception in Save's
// empty-session skip: an empty session that was explicitly renamed
// (RenameSession / session_rename tool) must still be persisted, or the
// rename would be silently dropped. Plain empty sessions (the /new bloat
// case) must stay skipped.
func TestSavePersistsRenamedEmptySession(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)

	// A renamed empty session: no messages, deliberate label.
	if err := store.Save("sess-renamed", agent.SessionSnapshot{
		WorkingDir: dir,
		Label:      "My Custom Title",
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := store.LoadInWorkingDir(dir, "sess-renamed")
	if err != nil {
		t.Fatalf("renamed empty session must be persisted: %v", err)
	}
	if snap.Label != "My Custom Title" {
		t.Fatalf("label=%q, want %q", snap.Label, "My Custom Title")
	}

	// An anonymous empty session is still skipped (no file, no index entry).
	if err := store.Save("sess-anon", agent.SessionSnapshot{WorkingDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadInWorkingDir(dir, "sess-anon"); err == nil {
		t.Fatal("anonymous empty session should not be persisted")
	}
}

// TestSessionLabelLegacyTruncationStillMigrated pins the other half of the
// trade-off: a legacy 50-char stored label that is a prefix of the full
// message is still migrated to the untruncated text.
func TestSessionLabelLegacyTruncationStillMigrated(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "implement session commands with labels and keep going"}}
	derived := llm.SessionLabel(msgs)
	legacy := derived[:legacyLabelMaxLen]
	if got := sessionLabel(msgs, legacy); got != derived {
		t.Fatalf("legacy truncated label = %q, want migrated to %q", got, derived)
	}
	// A rename shorter than the legacy length must NOT be regenerated.
	if got := sessionLabel(msgs, "Short title"); got != "Short title" {
		t.Fatalf("rename = %q, want kept, got %q", "Short title", got)
	}
	// Empty stored label falls back to the message-derived label.
	if got := sessionLabel(msgs, ""); got != derived {
		t.Fatalf("empty stored = %q, want %q", got, derived)
	}
}
