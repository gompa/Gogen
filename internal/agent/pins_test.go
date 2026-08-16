package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestPinLastUserSkipsCompactionSummaries pins the pin semantics for
// conversations whose last user-role message is a compaction summary
// (legacy sessions stored summaries with a user role): the pin must land on
// the last real user message, never on a summary.
func TestPinLastUserSkipsCompactionSummaries(t *testing.T) {
	p := NewPinManager()
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "work"},
		{Role: "user", Content: "real last user message"},
		{Role: "assistant", Content: "more work"},
		{Role: "user", Content: contextmgr.SummaryPrefix + "legacy summary"},
	}
	p.PinLastUser(msgs)
	if !p.IsPinned(2) {
		t.Fatalf("expected the last real user message (index 2) to be pinned, got %v", p.PinnedIndices())
	}
	if p.IsPinned(4) {
		t.Fatalf("summary message (index 4) must not be pinned, got %v", p.PinnedIndices())
	}
}

// TestPinLastUserNoRealUserMessage pins the no-op case: when every user
// message is a compaction summary, nothing is pinned.
func TestPinLastUserNoRealUserMessage(t *testing.T) {
	p := NewPinManager()
	msgs := []llm.Message{
		{Role: "user", Content: contextmgr.SummaryPrefix + "legacy summary"},
	}
	p.PinLastUser(msgs)
	if len(p.PinnedIndices()) != 0 {
		t.Fatalf("expected no pin, got %v", p.PinnedIndices())
	}
}

// TestListPinsTruncatesRuneSafe pins the display truncation: a long pinned
// message is cut at 80 RUNES (never mid-character), so the tool result stays
// valid UTF-8.
func TestListPinsTruncatesRuneSafe(t *testing.T) {
	p := NewPinManager()
	// 85 multi-byte runes: a byte-slice cut at 80 would split a rune.
	content := ""
	for i := 0; i < 85; i++ {
		content += "é"
	}
	msgs := []llm.Message{{Role: "user", Content: content}}
	p.PinLastUser(msgs)

	out := p.ListPins(msgs)
	want := string([]rune(content)[:80]) + "…"
	if !strings.Contains(out, want) {
		t.Fatalf("ListPins output = %q, want it to contain the rune-safe 80-rune cut %q", out, want)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("ListPins output is not valid UTF-8: %q", out)
	}
}
