package tui

import (
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// renderMessages converts a slice of LLM messages into styled display lines
// suitable for the chat viewport. All messages are rendered (no truncation;
// the viewport handles scrolling).
func renderMessages(messages []llm.Message, workingDir string, modelName string, mode string) []string {
	var lines []string
	var tcMap map[string]llm.ToolCall
	tcMapNeeded := false

	// Header
	if modelName != "" {
		header := fmt.Sprintf("GoGen — %s (%s)", workingDir, mode)
		lines = append(lines, DimStyle.Render(header))
		lines = append(lines, "")
	}

	if len(messages) == 0 {
		return lines
	}

	// Build tool-call ID → ToolCall map so tool results show names and diffs.
	for _, msg := range messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			tcMapNeeded = true
			break
		}
	}
	if tcMapNeeded {
		tcMap = make(map[string]llm.ToolCall)
		for _, msg := range messages {
			if msg.Role == "assistant" {
				for _, tc := range msg.ToolCalls {
					tcMap[tc.ID] = tc
				}
			}
		}
	}

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			if msg.Content != "" {
				label := UserStyle.Render(userLabel)
				lines = append(lines, label+" "+msg.Content)
			}
		case "assistant":
			// Always render thinking block when reasoning is present.
			if msg.Reasoning != "" {
				lines = append(lines, ThinkingTagStyle.Render("<thinking>"+msg.Reasoning+"</thinking>"))
			}
			// Skip the assistant content line when it duplicates the reasoning
			// (happens when the model only emitted reasoning with no content).
			if msg.Content != "" && msg.Content != msg.Reasoning {
				label := AssistantStyle.Render(assistantLabel)
				lines = append(lines, label+" "+msg.Content)
			}
			for _, tc := range msg.ToolCalls {
				prefix := ToolCallStyle.Render("  →")
				argStr := formatToolArgs(tc.Args)
				if argStr == "" {
					lines = append(lines, prefix+" "+tc.Name)
				} else {
					lines = append(lines, prefix+" "+tc.Name+" "+ToolCallArgsStyle.Render(argStr))
				}
			}
		case "tool":
			if msg.Content != "" {
				lines = append(lines, renderToolResult(msg, tcMap)...)
			}
		}
	}

	return lines
}

// renderToolResult renders a single tool result in a style that matches the
// live-streaming rendering (handleStreamToolResult), including the tool name,
// success/failure status, diff blocks, and result summaries.
func renderToolResult(msg llm.Message, tcMap map[string]llm.ToolCall) []string {
	var lines []string

	// Resolve the tool name from the matching tool call.
	toolName := ""
	var tc llm.ToolCall
	hasTC := false
	if tcMap != nil {
		tc, hasTC = tcMap[msg.ToolCallID]
		if hasTC {
			toolName = tc.Name
		}
	}

	// Detect success/failure heuristically from the stored message content.
	success := !strings.HasPrefix(strings.TrimSpace(msg.Content), "Error:")
	status := "ok"
	statusStyle := ToolResultOKStyle
	if !success {
		status = "failed"
		statusStyle = ToolResultFailStyle
	}

	mark := ToolResultMarkStyle.Render("  ↳")
	lines = append(lines, fmt.Sprintf("%s %s  %s", mark, toolName, statusStyle.Render(status)))

	// show_diff: when the result looks like a unified diff, render it coloured
	// and skip the summary (matches the live path).
	if toolName == "show_diff" && isDiffContent(msg.Content) {
		if rendered := renderDiff(msg.Content); rendered != "" {
			lines = append(lines, DiffMetaStyle.Render("  ╭─ diff ─"))
			for _, line := range strings.Split(rendered, "\n") {
				lines = append(lines, line)
			}
			lines = append(lines, DiffMetaStyle.Render("  ╰───────"))
		}
		return lines
	}

	// patch_file: render the diff that was passed as a tool argument (when
	// available). Like the live path, skip the summary when a diff is shown.
	if toolName == "patch_file" && hasTC {
		if diff, ok := tc.Args["diff"].(string); ok && diff != "" {
			if rendered := renderDiff(diff); rendered != "" {
				lines = append(lines, DiffMetaStyle.Render("  ╭─ diff ─"))
				for _, line := range strings.Split(rendered, "\n") {
					lines = append(lines, line)
				}
				lines = append(lines, DiffMetaStyle.Render("  ╰───────"))
			}
			return lines
		}
	}

	// Summary for everything else (matches the non-verbose live path).
	summary := summarizeResult(msg.Content, success)
	lines = append(lines, DimStyle.Render("  "+summary))
	return lines
}

// formatToolArgs formats tool call arguments for display.
func formatToolArgs(args map[string]interface{}) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		val := fmt.Sprintf("%v", v)
		if len(val) > 80 {
			val = truncateRunes(val, 77) + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, val))
	}
	return strings.Join(parts, " ")
}

// extractDiffValue extracts the "diff" string value from raw (possibly incomplete)
// JSON tool-call arguments. It handles escaped newlines and quotes.
// Returns the unescaped diff text and true if any content was found.
func extractDiffValue(rawJSON string) (string, bool) {
	idx := strings.Index(rawJSON, `"diff"`)
	if idx < 0 {
		return "", false
	}
	// Skip past "diff", optional whitespace, :, optional whitespace, opening "
	rest := rawJSON[idx+6:]
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) == 0 || rest[0] != ':' {
		return "", false
	}
	rest = rest[1:]
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) == 0 || rest[0] != '"' {
		return "", false
	}
	rest = rest[1:] // skip opening "

	var buf strings.Builder
	i := 0
	for i < len(rest) {
		if rest[i] == '\\' && i+1 < len(rest) {
			switch rest[i+1] {
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			case '"':
				buf.WriteByte('"')
			case '\\':
				buf.WriteByte('\\')
			case 'r':
				// \r is a no-op in the diff; skip
			default:
				buf.WriteByte(rest[i])
				buf.WriteByte(rest[i+1])
			}
			i += 2
		} else if rest[i] == '"' {
			// Unescaped quote — end of string
			return buf.String(), true
		} else {
			buf.WriteByte(rest[i])
			i++
		}
	}
	return buf.String(), buf.Len() > 0
}

// formatArgsCompact formats args like formatToolArgs but skips any key whose
// value exceeds maxLen. When no keys survive, returns "".
func formatArgsCompact(rawJSON string, maxLen int) string {
	args, err := parseInlineJSONArgs(rawJSON)
	if err != nil || len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		if k == "diff" {
			continue
		}
		val := fmt.Sprintf("%v", v)
		if len(val) > maxLen {
			val = truncateRunes(val, maxLen-3) + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, val))
	}
	return strings.Join(parts, " ")
}
