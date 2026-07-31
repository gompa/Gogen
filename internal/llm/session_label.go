package llm

import (
	"strings"
)

// SessionLabel returns a normalized preview of the first user message.
// Truncation is left to the client (CSS text-overflow: ellipsis) so the
// full text is available when the sidebar is wide enough.
func SessionLabel(messages []Message) string { return FirstUserMessage(messages) }

// FirstUserMessage returns the content of the first user message, untruncated,
// with whitespace normalized (newlines → spaces, runs collapsed).
func FirstUserMessage(messages []Message) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		s := strings.TrimSpace(m.Content)
		if s == "" {
			continue
		}
		s = strings.ReplaceAll(s, "\n", " ")
		return strings.Join(strings.Fields(s), " ")
	}
	return ""
}
