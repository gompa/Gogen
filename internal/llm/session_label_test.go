package llm

import (
	"strings"
	"testing"
)

func TestSessionLabelTruncates(t *testing.T) {
	long := strings.Repeat("x", 80)
	got := SessionLabel([]Message{{Role: "user", Content: long}}, 50)
	if len([]rune(got)) != 50 {
		t.Fatalf("got len %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}
