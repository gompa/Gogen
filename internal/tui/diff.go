package tui

import (
	"fmt"
	"strings"
)

// renderDiffLine applies the diff ANSI coloring to a single line: '+' lines
// green, '-' red, '@@' hunk headers cyan, '---'/'+++' file headers
// yellow+bold, everything else unstyled. Empty lines render as "".
func renderDiffLine(line string) string {
	if len(line) == 0 {
		return ""
	}
	switch line[0] {
	case '+':
		if strings.HasPrefix(line, "+++ ") {
			return DiffMetaStyle.Render(line)
		}
		return DiffAddStyle.Render(line)
	case '-':
		if strings.HasPrefix(line, "--- ") {
			return DiffMetaStyle.Render(line)
		}
		return DiffDelStyle.Render(line)
	case '@':
		if strings.HasPrefix(line, "@@") {
			return DiffHunkStyle.Render(line)
		}
	}
	return line
}

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
		out.WriteString(renderDiffLine(line))
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

// diffBlock renders a unified diff inside the shared "╭─ diff ─" /
// "╰───────" frame used by both the live streaming and history render
// paths. Returns nil when diff is empty or whitespace-only, so callers can
// append the result unconditionally.
func diffBlock(diff string) []string {
	rendered := renderDiff(diff)
	if rendered == "" {
		return nil
	}
	lines := []string{DiffMetaStyle.Render("  ╭─ diff ─")}
	lines = append(lines, strings.Split(rendered, "\n")...)
	lines = append(lines, DiffMetaStyle.Render("  ╰───────"))
	return lines
}

// toolResultStatusLine renders the "↳ name  ok|failed" header line for a
// tool result, shared by the live streaming and history render paths.
func toolResultStatusLine(name string, success bool) string {
	status := "ok"
	statusStyle := ToolResultOKStyle
	if !success {
		status = "failed"
		statusStyle = ToolResultFailStyle
	}
	mark := ToolResultMarkStyle.Render("  ↳")
	return fmt.Sprintf("%s %s  %s", mark, name, statusStyle.Render(status))
}
