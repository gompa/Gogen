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
