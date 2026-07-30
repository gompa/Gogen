package llm

import (
	"strings"
)

// DefaultSessionLabelMaxLen is kept for backward compatibility but no longer
// enforced — the label is returned untruncated and CSS text-overflow: ellipsis
// handles dynamic truncation on the client side.
const DefaultSessionLabelMaxLen = 1 << 30 // effectively unlimited

// SessionLabel returns a normalized preview of the first user message.
// Truncation is left to the client (CSS text-overflow: ellipsis) so the
// full text is available when the sidebar is wide enough.
func SessionLabel(messages []Message, maxLen int) string { return FirstUserMessage(messages) }

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
