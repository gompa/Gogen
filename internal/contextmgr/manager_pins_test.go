package contextmgr

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestCompactPinnedPreservesPinnedMessage(t *testing.T) {
	provider := &stubProvider{summary: "middle summary"}
	m := NewManager(provider, Settings{KeepRecentMessages: 2, ContextLimit: 100000, CompactThreshold: 0.01})
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "PIN ME important constraint"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "later1"},
		{Role: "assistant", Content: "a3"},
		{Role: "user", Content: "later2"},
		{Role: "assistant", Content: "a4"},
	}
	pinned := map[int]struct{}{2: {}}
	out, newPins, err := m.CompactPinned(context.Background(), msgs, pinned)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range out {
		if strings.Contains(msg.Content, "PIN ME") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pinned message missing from compacted history: %+v", out)
	}
	if len(newPins) == 0 {
		t.Fatal("expected remapped pins")
	}
	for idx := range newPins {
		if idx < 0 || idx >= len(out) {
			t.Fatalf("remapped pin %d out of range", idx)
		}
		if !strings.Contains(out[idx].Content, "PIN ME") {
			t.Fatalf("remapped pin %d content = %q", idx, out[idx].Content)
		}
	}
}
