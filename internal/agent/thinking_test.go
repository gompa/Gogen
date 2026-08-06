package agent

import (
	"reflect"
	"testing"
)

// TestValidThinkingLevelsCanonical pins the user-facing level set: only
// levels with a distinct reasoning_effort value on the wire are exposed,
// in weakest → strongest order. The folded names (minimal/xhigh/max) must
// not appear — they are parse-only aliases now.
func TestValidThinkingLevelsCanonical(t *testing.T) {
	want := []ThinkingLevel{ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh}
	if got := ValidThinkingLevels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidThinkingLevels() = %v, want %v", got, want)
	}
}

// TestParseThinkingLevelFoldsAliases verifies the old minimal/xhigh/max names
// still parse (sessions persisted them before the fold) and normalize to the
// canonical low/high levels, so a restored session shows the folded level
// rather than a level that no longer exists.
func TestParseThinkingLevelFoldsAliases(t *testing.T) {
	cases := map[string]struct {
		want ThinkingLevel
		ok   bool
	}{
		"off":     {ThinkingOff, true},
		"0":       {ThinkingOff, true},
		"low":     {ThinkingLow, true},
		"minimal": {ThinkingLow, true},
		"min":     {ThinkingLow, true},
		"medium":  {ThinkingMedium, true},
		"med":     {ThinkingMedium, true},
		"high":    {ThinkingHigh, true},
		"xhigh":   {ThinkingHigh, true},
		"x-high":  {ThinkingHigh, true},
		"max":     {ThinkingHigh, true},
		"bogus":   {ThinkingOff, false},
		"":        {ThinkingOff, false},
		" Max ":   {ThinkingHigh, true}, // trimmed + case-insensitive
	}
	for in, tc := range cases {
		got, ok := ParseThinkingLevel(in)
		if ok != tc.ok {
			t.Fatalf("ParseThinkingLevel(%q) ok = %v, want %v", in, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if got != tc.want {
			t.Fatalf("ParseThinkingLevel(%q) = %q, want %q", in, got, tc.want)
		}
		// The folded result must display as the canonical level — never as
		// "Mi"/"XH"/"Max" labels that no longer exist.
		if got.Label() != tc.want.Label() || got.ShortLabel() != tc.want.ShortLabel() {
			t.Fatalf("ParseThinkingLevel(%q) displays as %q/%q, want %q/%q",
				in, got.Label(), got.ShortLabel(), tc.want.Label(), tc.want.ShortLabel())
		}
	}
}

// TestFoldedLevelsNotDisplayed guards the display surfaces: the folded names
// must not appear in any label/short-label/detail metadata.
func TestFoldedLevelsNotDisplayed(t *testing.T) {
	for _, name := range []string{"minimal", "xhigh", "max", "Mi", "XH", "Max"} {
		for _, level := range ValidThinkingLevels() {
			for _, s := range []string{level.Label(), level.ShortLabel(), level.Details()} {
				if s == name {
					t.Fatalf("folded level name %q leaked into display of %q (%q)", name, level, s)
				}
			}
		}
	}
}
