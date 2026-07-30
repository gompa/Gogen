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

// thinkingInfo holds display and parsing metadata for a thinking level.
type thinkingInfo struct {
	label      string
	shortLabel string
	details    string
	aliases    []string // parse aliases (lowercase)
}

// thinkingLevels is the single source of truth for all level metadata.
var thinkingLevels = map[ThinkingLevel]thinkingInfo{
	ThinkingOff: {
		label:      "Off",
		shortLabel: "",
		details:    "No reasoning",
		aliases:    []string{"off", "0"},
	},
	ThinkingMinimal: {
		label:      "Minimal",
		shortLabel: "Mi",
		details:    "Very brief reasoning",
		aliases:    []string{"minimal", "min"},
	},
	ThinkingLow: {
		label:      "Low",
		shortLabel: "L",
		details:    "Light reasoning",
		aliases:    []string{"low"},
	},
	ThinkingMedium: {
		label:      "Medium",
		shortLabel: "M",
		details:    "Moderate reasoning",
		aliases:    []string{"medium", "med"},
	},
	ThinkingHigh: {
		label:      "High",
		shortLabel: "H",
		details:    "Deep reasoning",
		aliases:    []string{"high"},
	},
	ThinkingXHigh: {
		label:      "Extra high",
		shortLabel: "XH",
		details:    "Extra-deep reasoning",
		aliases:    []string{"xhigh", "x-high"},
	},
	ThinkingMax: {
		label:      "Maximum",
		shortLabel: "Max",
		details:    "Maximum reasoning",
		aliases:    []string{"max"},
	},
}

// ValidThinkingLevels returns all supported thinking levels.
func ValidThinkingLevels() []ThinkingLevel {
	all := make([]ThinkingLevel, 0, len(thinkingLevels))
	for level := range thinkingLevels {
		all = append(all, level)
	}
	return all
}

// ParseThinkingLevel parses a thinking level string. Returns false on unknown input.
func ParseThinkingLevel(s string) (ThinkingLevel, bool) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	for level, info := range thinkingLevels {
		for _, alias := range info.aliases {
			if normalized == alias {
				return level, true
			}
		}
	}
	return ThinkingOff, false
}

// ShortLabel returns a compact label for display in toolbars (empty for "off").
func (l ThinkingLevel) ShortLabel() string {
	if info, ok := thinkingLevels[l]; ok {
		return info.shortLabel
	}
	return ""
}

// Label returns a user-friendly label.
func (l ThinkingLevel) Label() string {
	if info, ok := thinkingLevels[l]; ok {
		return info.label
	}
	return "Off"
}

// Details returns a longer description of the thinking level.
func (l ThinkingLevel) Details() string {
	if info, ok := thinkingLevels[l]; ok {
		return info.details
	}
	return ""
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
