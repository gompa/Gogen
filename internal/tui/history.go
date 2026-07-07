package tui

import (
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// renderMessages converts a slice of LLM messages into styled display lines
// suitable for the chat viewport.
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

	// Count user+assistant messages for the "showing last N" truncation
	type displayMsg struct {
		role    string
		content string
	}
	total := 0
	for _, m := range messages {
		if (m.Role == "user" || m.Role == "assistant") && m.Content != "" {
			total++
		}
	}

	var keep []displayMsg
	users, assts := 0, 0
	for i := len(messages) - 1; i >= 0 && (users < 2 || assts < 2); i-- {
		msg := messages[i]
		if msg.Role == "user" && msg.Content != "" && users < 2 {
			users++
			keep = append(keep, displayMsg{role: "user", content: msg.Content})
		} else if msg.Role == "assistant" && msg.Content != "" && assts < 2 {
			assts++
			keep = append(keep, displayMsg{role: "assistant", content: msg.Content})
		}
	}
	// Reverse back
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}

	truncated := total > len(keep)
	if truncated {
		lines = append(lines, DimStyle.Render(fmt.Sprintf("⋮ (%d messages, showing last %d)", total, len(keep))))
	}

	for _, h := range keep {
		if h.role == "assistant" {
			label := AssistantStyle.Render(assistantLabel)
			content := DimStyle.Render(h.content)
			lines = append(lines, label+" "+content)
		} else {
			lines = append(lines, h.content)
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
