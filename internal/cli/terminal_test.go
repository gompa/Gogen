package cli

import (
	"strings"
	"testing"
)

func TestFormatRightAlignedDimLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("COLUMNS", "40")

	line := formatRightAlignedDimLine("context: 9k / 200k (4%)")
	if !strings.HasPrefix(line, strings.Repeat(" ", 8)) {
		t.Fatalf("expected right-aligned padding, got %q", line)
	}
	if !strings.HasSuffix(line, "context: 9k / 200k (4%)") {
		t.Fatalf("expected text preserved at end, got %q", line)
	}
}

func TestTerminalColumnsFromEnv(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	if got := terminalColumns(); got != 120 {
		t.Fatalf("got %d want 120", got)
	}
}
