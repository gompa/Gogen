package agent

import (
	"fmt"
	"strings"
)

// ThinkingLevel controls how much reasoning/thinking the model performs.
// The zero value ("off") means no thinking parameter is sent to the API.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
	ThinkingMax     ThinkingLevel = "max"
)

// ValidThinkingLevels returns all supported thinking levels.
func ValidThinkingLevels() []ThinkingLevel {
	return []ThinkingLevel{
		ThinkingOff,
		ThinkingMinimal,
		ThinkingLow,
		ThinkingMedium,
		ThinkingHigh,
		ThinkingXHigh,
		ThinkingMax,
	}
}

// ParseThinkingLevel parses a thinking level string. Returns false on unknown input.
func ParseThinkingLevel(s string) (ThinkingLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "0":
		return ThinkingOff, true
	case "minimal", "min":
		return ThinkingMinimal, true
	case "low":
		return ThinkingLow, true
	case "medium", "med":
		return ThinkingMedium, true
	case "high":
		return ThinkingHigh, true
	case "xhigh", "x-high":
		return ThinkingXHigh, true
	case "max":
		return ThinkingMax, true
	default:
		return ThinkingOff, false
	}
}

// ShortLabel returns a compact label for display in toolbars (empty for "off").
func (l ThinkingLevel) ShortLabel() string {
	switch l {
	case ThinkingOff:
		return ""
	case ThinkingMinimal:
		return "Mi"
	case ThinkingLow:
		return "L"
	case ThinkingMedium:
		return "M"
	case ThinkingHigh:
		return "H"
	case ThinkingXHigh:
		return "XH"
	case ThinkingMax:
		return "Max"
	default:
		return ""
	}
}

// Label returns a user-friendly label.
func (l ThinkingLevel) Label() string {
	switch l {
	case ThinkingOff:
		return "Off"
	case ThinkingMinimal:
		return "Minimal"
	case ThinkingLow:
		return "Low"
	case ThinkingMedium:
		return "Medium"
	case ThinkingHigh:
		return "High"
	case ThinkingXHigh:
		return "Extra high"
	case ThinkingMax:
		return "Maximum"
	default:
		return "Off"
	}
}

// Details returns a longer description of the thinking level.
func (l ThinkingLevel) Details() string {
	switch l {
	case ThinkingOff:
		return "No reasoning"
	case ThinkingMinimal:
		return "Very brief reasoning"
	case ThinkingLow:
		return "Light reasoning"
	case ThinkingMedium:
		return "Moderate reasoning"
	case ThinkingHigh:
		return "Deep reasoning"
	case ThinkingXHigh:
		return "Extra-deep reasoning"
	case ThinkingMax:
		return "Maximum reasoning"
	default:
		return ""
	}
}

// SetThinkingLevel sets the agent's thinking level and persists the session.
func (a *Agent) SetThinkingLevel(l ThinkingLevel) {
	a.ThinkingLevel = l
	a.FlushSession()
}

// HandleThinkingCommand processes /think commands.
func (a *Agent) HandleThinkingCommand(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "/think" || trimmed == "think" {
		return fmt.Sprintf("Thinking level: %s (%s)", a.ThinkingLevel.Label(), a.ThinkingLevel.Details()), true
	}
	if strings.HasPrefix(trimmed, "/think ") || strings.HasPrefix(trimmed, "think ") {
		parts := strings.SplitN(trimmed, " ", 2)
		if len(parts) < 2 {
			return "", false
		}
		level, ok := ParseThinkingLevel(parts[1])
		if !ok {
			valid := make([]string, len(ValidThinkingLevels()))
			for i, l := range ValidThinkingLevels() {
				valid[i] = string(l)
			}
			return fmt.Sprintf("Unknown thinking level %q. Valid levels: %s", parts[1], strings.Join(valid, ", ")), true
		}
		a.SetThinkingLevel(level)
		if level == ThinkingOff {
			return "Thinking level set to off (no thinking parameter sent to the API).", true
		}
		return fmt.Sprintf("Thinking level set to %s (%s).", level.Label(), level.Details()), true
	}
	return "", false
}
