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

	// Header
	if modelName != "" {
		header := fmt.Sprintf("GoGen — %s (%s)", workingDir, mode)
		lines = append(lines, DimStyle.Render(header))
		lines = append(lines, "")
	}

	if len(messages) == 0 {
		return lines
	}

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			if msg.Content != "" {
				label := UserStyle.Render(userLabel)
				lines = append(lines, label+" "+msg.Content)
			}
		case "assistant":
			if msg.Content != "" {
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
				mark := ToolResultMarkStyle.Render("  ↳")
				trimmed := strings.TrimSpace(msg.Content)
				if len(trimmed) > 200 {
					trimmed = trimmed[:197] + "..."
				}
				lines = append(lines, mark+" "+DimStyle.Render(trimmed))
			}
		}
	}

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
			val = val[:77] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, val))
	}
	return strings.Join(parts, " ")
}
