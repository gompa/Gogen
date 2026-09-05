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
				lines = append(lines, renderStyledBlock(ThinkingTagStyle, "<thinking>"+msg.Reasoning+"</thinking>"))
			}
			switch {
			case msg.Content != "":
				label := AssistantStyle.Render(assistantLabel)
				lines = append(lines, label+" "+msg.Content)
			case msg.Refusal != "":
				label := AssistantStyle.Render(assistantLabel)
				lines = append(lines, label+" "+msg.Refusal)
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

	lines = append(lines, toolResultStatusLine(toolName, success))

	// show_diff: when the result looks like a unified diff, render it coloured
	// and skip the summary (matches the live path).
	if toolName == "show_diff" && isDiffContent(msg.Content) {
		lines = append(lines, diffBlock(msg.Content)...)
		return lines
	}

	// patch_file: render the diff that was passed as a tool argument (when
	// available). Like the live path, skip the summary when a diff is shown.
	if toolName == "patch_file" && hasTC {
		if diff, ok := tc.Args["diff"].(string); ok && diff != "" {
			lines = append(lines, diffBlock(diff)...)
			return lines
		}
	}

	// Summary for everything else (matches the non-verbose live path).
	summary := summarizeResult(msg.Content, success)
	lines = append(lines, DimStyle.Render("  "+summary))
	return lines
}

// formatArgsMap renders tool-call arguments as "key=value" pairs joined by
// spaces, truncating values longer than maxLen (in runes) with an ellipsis.
// skip, when non-nil, omits keys for which it returns true. When no key
// survives, returns "".
func formatArgsMap(args map[string]any, maxLen int, skip func(string) bool) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		if skip != nil && skip(k) {
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

// formatToolArgs formats tool call arguments for display.
func formatToolArgs(args map[string]any) string {
	return formatArgsMap(args, 80, nil)
}

// formatArgsCompact renders inline JSON tool-call args like formatToolArgs,
// skipping the diff key; values longer than maxLen are truncated. When no
// keys survive, returns "". Takes a byte slice so streaming callers can pass
// the append-grown args buffer without an O(N) string materialization.
func formatArgsCompact(rawJSON []byte, maxLen int) string {
	args, err := parseInlineJSONArgs(rawJSON)
	if err != nil || len(args) == 0 {
		return ""
	}
	return formatArgsMap(args, maxLen, func(k string) bool { return k == "diff" })
}
