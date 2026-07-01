package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatToolErrorIncludesOutput(t *testing.T) {
	got := formatToolError("line one\nline two", errors.New("execution error: exit status 1"))
	if !strings.Contains(got, "Error: execution error: exit status 1") {
		t.Fatalf("missing error text: %q", got)
	}
	if !strings.Contains(got, "Output:") || !strings.Contains(got, "line one") {
		t.Fatalf("missing command output: %q", got)
	}
}

func TestFormatToolErrorWithoutOutput(t *testing.T) {
	got := formatToolError("", errors.New("command timed out"))
	if got != "Error: command timed out" {
		t.Fatalf("got %q", got)
	}
}
