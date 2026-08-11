package agent

import (
	"testing"
)

// TestNormalizeThinkingLevel pins the input canonicalization contract: input
// is trimmed and lowercased, and blank input normalizes to "" (the "off"
// sentinel). There is no vocabulary rejection — any non-blank token is a
// literal reasoning_effort value whose validity is decided by the current
// model's accepted set (see AvailableThinkingLevels), never by a fixed table.
func TestNormalizeThinkingLevel(t *testing.T) {
	cases := map[string]ThinkingLevel{
		"off":    ThinkingOff,
		"OFF":    ThinkingOff,
		" off ":  ThinkingOff,
		"low":    ThinkingLow,
		"medium": ThinkingMedium,
		"high":   ThinkingHigh,
		"max":    ThinkingLevel("max"),   // open vocabulary: literal, never folded
		"xhigh":  ThinkingLevel("xhigh"), // open vocabulary: literal, never folded
		" Max ":  ThinkingLevel("max"),   // trimmed + case-insensitive
		"min":    ThinkingLevel("min"),   // no alias folding — literal
		"bogus":  ThinkingLevel("bogus"), // unknown to gogen, still a literal
		"":       "",
		"   ":    "",
	}
	for in, want := range cases {
		if got := NormalizeThinkingLevel(in); got != want {
			t.Fatalf("NormalizeThinkingLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDefaultLevelLabels pins the display metadata of the closed default set
// and the derived labels for values outside it: every accepted value must
// render — defaults from the table, open values by title-case.
func TestDefaultLevelLabels(t *testing.T) {
	cases := []struct {
		level      ThinkingLevel
		label      string
		shortLabel string
	}{
		{ThinkingOff, "Off", ""},
		{ThinkingLow, "Low", "L"},
		{ThinkingMedium, "Medium", "M"},
		{ThinkingHigh, "High", "H"},
		{ThinkingLevel("max"), "Max", "M"}, // derived (not in default set)
		{ThinkingLevel("xhigh"), "Xhigh", "X"},
		{ThinkingLevel("none"), "None", "N"},
		{"", "Off", ""}, // unset renders as Off
	}
	for _, tc := range cases {
		if got := tc.level.Label(); got != tc.label {
			t.Fatalf("level %q Label = %q, want %q", tc.level, got, tc.label)
		}
		if got := tc.level.ShortLabel(); got != tc.shortLabel {
			t.Fatalf("level %q ShortLabel = %q, want %q", tc.level, got, tc.shortLabel)
		}
	}
}

// TestLabelNeverEmpty guards the display surfaces: a stored value (even one
// inactive for the current model) must render a non-empty label instead of
// leaking an empty string.
func TestLabelNeverEmpty(t *testing.T) {
	for _, l := range []ThinkingLevel{ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingLevel("max"), ThinkingLevel("xhigh"), ThinkingLevel("minimal")} {
		if l.Label() == "" {
			t.Fatalf("level %q has empty Label", l)
		}
	}
	if ThinkingOff.Label() == "" {
		t.Fatal("off Label is empty")
	}
}
