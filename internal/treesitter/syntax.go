package treesitter

import (
	"fmt"
	"path/filepath"
	"strings"
)

// FormatCheck returns a non-empty note when path/content has tree-sitter syntax issues.
func FormatCheck(path string, content []byte) string {
	if !Enabled() {
		return ""
	}
	issues := Check(path, content)
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Syntax check (")
	b.WriteString(filepath.Base(path))
	b.WriteString("):\n")
	for _, issue := range issues {
		fmt.Fprintf(&b, "  line %d: %s\n", issue.Line, issue.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Issue describes a syntax problem at a source line.
type Issue struct {
	Line    int
	Message string
}

// Check parses content when path has a supported extension.
func Check(path string, content []byte) []Issue {
	if !Enabled() {
		return nil
	}
	return checkSupported(path, content)
}
