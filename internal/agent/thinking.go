package agent

import (
	"fmt"
	"strings"
)

// ThinkingLevel controls how much reasoning/thinking the model performs.
// The zero value ("off") means no thinking parameter is sent to the API.
// Only levels that map to a distinct reasoning_effort value on the wire are
// exposed (off/low/medium/high); the older minimal/xhigh/max names are still
// accepted as parse aliases and fold onto low/high (see thinkingLevels).
type ThinkingLevel string

const (
	ThinkingOff    ThinkingLevel = "off"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
)

// thinkingInfo holds display and parsing metadata for a thinking level.
type thinkingInfo struct {
	label      string
	shortLabel string
	details    string
	aliases    []string // parse aliases (lowercase)
}

// thinkingLevelOrder is the canonical level ordering (weakest → strongest),
// used by ValidThinkingLevels so listings (help text, /think error messages)
// are deterministic instead of map-iteration order.
var thinkingLevelOrder = []ThinkingLevel{
	ThinkingOff,
	ThinkingLow,
	ThinkingMedium,
	ThinkingHigh,
}

// thinkingLevels is the single source of truth for all level metadata.
var thinkingLevels = map[ThinkingLevel]thinkingInfo{
	ThinkingOff: {
		label:      "Off",
		shortLabel: "",
		details:    "No reasoning",
		aliases:    []string{"off", "0"},
	},
	ThinkingLow: {
		label:      "Low",
		shortLabel: "L",
		details:    "Light reasoning",
		aliases:    []string{"low", "minimal", "min"},
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
		// "xhigh"/"max" are kept as parse aliases for sessions that
		// persisted them before the fold; they normalize to "high".
		aliases: []string{"high", "xhigh", "x-high", "max"},
	},
}

// ValidThinkingLevels returns all supported thinking levels in canonical
// (weakest → strongest) order.
func ValidThinkingLevels() []ThinkingLevel {
	all := make([]ThinkingLevel, len(thinkingLevelOrder))
	copy(all, thinkingLevelOrder)
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

// SetThinkingLevel sets the agent's thinking level, syncs the provider's
// reasoning-effort state so the two can never diverge, and persists the
// session. The level field itself is written under statsMu (see
// Agent.ModeAndThinkingLevel — config snapshots read it without the turn
// lock, so a mid-turn attach never blocks); the provider sync runs outside
// the lock.
func (a *Agent) SetThinkingLevel(l ThinkingLevel) {
	a.statsMu.Lock()
	a.ThinkingLevel = l
	a.statsMu.Unlock()
	if a.Provider != nil {
		a.Provider.SetThinkingLevel(string(l))
	}
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
