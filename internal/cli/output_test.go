package cli

import (
	"strings"
	"testing"
)

func TestFormatToolArgs(t *testing.T) {
	got := formatToolArgs(map[string]interface{}{
		"path":      "main.go",
		"recursive": true,
	})
	if !strings.Contains(got, `path="main.go"`) || !strings.Contains(got, `recursive="true"`) {
		t.Fatalf("formatToolArgs: %q", got)
	}
}

func TestSummarizeToolResult(t *testing.T) {
	tests := []struct {
		result  string
		success bool
		want    string
	}{
		{"", true, "(empty)"},
		{"permission denied", false, "permission denied (17 chars)"},
		{"one line", true, "one line"},
		{"line1\nline2\nline3", true, "(3 lines, 17 chars)"},
	}
	for _, tt := range tests {
		got := summarizeToolResult(tt.result, tt.success)
		if got != tt.want {
			t.Errorf("summarizeToolResult(%q, %v) = %q, want %q", tt.result, tt.success, got, tt.want)
		}
	}
}

func TestFormatToolResultBody(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := formatToolResultBody(long, 50, 0)
	if len(got) < 50 || !strings.Contains(got, "total chars") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
}
