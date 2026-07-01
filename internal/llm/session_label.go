package llm

import (
	"strings"
	"unicode/utf8"
)

const DefaultSessionLabelMaxLen = 50

// SessionLabel returns a short preview of the first user message.
func SessionLabel(messages []Message, maxLen int) string {
	if maxLen <= 0 {
		maxLen = DefaultSessionLabelMaxLen
	}
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		s := strings.TrimSpace(m.Content)
		if s == "" {
			continue
		}
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.Join(strings.Fields(s), " ")
		return truncateRunes(s, maxLen)
	}
	return ""
}

func truncateRunes(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	if maxLen <= 1 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}
