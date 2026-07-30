package llm

import (
	"strings"
	"testing"
)

func TestSessionLabelUntruncated(t *testing.T) {
	long := strings.Repeat("x", 80)
	got := SessionLabel([]Message{{Role: "user", Content: long}}, 50)
	// Label is now untruncated — CSS handles dynamic truncation.
	if got != long {
		t.Fatalf("expected full %d-char label, got %q (len %d)", len(long), got, len([]rune(got)))
	}
}
