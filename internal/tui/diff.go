package tui

import (
	"strings"
)

// renderDiff takes a unified diff string and returns it with ANSI coloring applied.
// Lines starting with '+' are colored green, '-' red, '@@' cyan, and
// '---'/'+++' headers are yellow+bold.
func renderDiff(diff string) string {
	if strings.TrimSpace(diff) == "" {
		return ""
	}
	lines := strings.Split(diff, "\n")
	var out strings.Builder
	for _, line := range lines {
		if len(line) == 0 {
			out.WriteByte('\n')
			continue
		}
		switch line[0] {
		case '+':
			if !strings.HasPrefix(line, "+++ ") {
				out.WriteString(DiffAddStyle.Render(line))
			} else {
				out.WriteString(DiffMetaStyle.Render(line))
			}
		case '-':
			if !strings.HasPrefix(line, "--- ") {
				out.WriteString(DiffDelStyle.Render(line))
			} else {
				out.WriteString(DiffMetaStyle.Render(line))
			}
		case '@':
			if strings.HasPrefix(line, "@@") {
				out.WriteString(DiffHunkStyle.Render(line))
			} else {
				out.WriteString(line)
			}
		default:
			out.WriteString(line)
		}
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}

// isDiffContent heuristically detects whether a string looks like a unified diff.
func isDiffContent(s string) bool {
	hasHunk := strings.Contains(s, "@@ -")
	hasHeader := strings.Contains(s, "--- ") || strings.Contains(s, "+++ ")
	return hasHunk || hasHeader
}
