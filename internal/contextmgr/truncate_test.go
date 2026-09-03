package contextmgr

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateRuneSafe verifies the byte cap never splits a UTF-8 rune and
// every boundary cut yields valid UTF-8.
func TestTruncateRuneSafe(t *testing.T) {
	const s = "日本語テキスト" // 7 runes × 3 bytes = 21 bytes
	for max := 1; max <= len(s); max++ {
		got := TruncateRuneSafe(s, max)
		if len(got) > max {
			t.Fatalf("max %d: result length %d exceeds max", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("max %d: result %q is not valid UTF-8", max, got)
		}
	}
	// Exact rune-boundary cuts: 日=3 bytes, 本=3 bytes, ...
	for _, tc := range []struct {
		max  int
		want string
	}{
		{3, "日"},
		{5, "日"}, // byte 5 lands inside 本 (bytes 3-5); backs off to 3
		{6, "日本"},
		{9, "日本語"},
		{21, s},
	} {
		if got := TruncateRuneSafe(s, tc.max); got != tc.want {
			t.Errorf("max %d: got %q, want %q", tc.max, got, tc.want)
		}
	}
	// No-op cases.
	if got := TruncateRuneSafe("abc", 3); got != "abc" {
		t.Errorf("max == len: got %q", got)
	}
	if got := TruncateRuneSafe("abc", 10); got != "abc" {
		t.Errorf("max > len: got %q", got)
	}
	if got := TruncateRuneSafe("abc", 0); got != "abc" {
		t.Errorf("max 0 (no cap): got %q", got)
	}
}

// TestTruncateRuneSafeParity verifies the zero-value options are exactly the
// plain rune-safe head cut (Truncate(s, max, {}) == TruncateRuneSafe(s, max)).
func TestTruncateRuneSafeParity(t *testing.T) {
	const s = "日本語テキスト" // 7 runes × 3 bytes = 21 bytes
	for max := 0; max <= len(s)+2; max++ {
		if got, want := Truncate(s, max, TruncateOptions{}), TruncateRuneSafe(s, max); got != want {
			t.Errorf("max %d: Truncate with zero options = %q, want %q", max, got, want)
		}
	}
}

// TestRuneSafeTailStart verifies the tail-cut counterpart of
// TruncateRuneSafe: for every byte cap, the tail starting at the returned
// offset is at most max bytes, valid UTF-8, and begins on a rune boundary
// (never with a split multi-byte character).
func TestRuneSafeTailStart(t *testing.T) {
	const s = "日本語テキスト" // 7 runes × 3 bytes = 21 bytes
	data := []byte(s)
	for max := 1; max <= len(data); max++ {
		start := RuneSafeTailStart(data, max)
		tail := data[start:]
		if len(tail) > max {
			t.Fatalf("max %d: tail length %d exceeds max", max, len(tail))
		}
		if !utf8.Valid(tail) {
			t.Fatalf("max %d: tail %q is not valid UTF-8", max, tail)
		}
		if len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			t.Fatalf("max %d: tail %q starts mid-rune", max, tail)
		}
	}
	// Exact cuts: the tail keeps the LAST bytes, backed off to a rune
	// boundary when the raw cut lands inside a rune.
	for _, tc := range []struct {
		max  int
		want string
	}{
		{3, "ト"},  // raw cut at byte 18 lands on a rune start
		{5, "ト"},  // raw cut at byte 16 lands inside ス (bytes 15-17); backs off to 18
		{6, "スト"}, // raw cut at byte 15 lands on a rune start
		{9, "キスト"},
		{21, s},
	} {
		if got := string(data[RuneSafeTailStart(data, tc.max):]); got != tc.want {
			t.Errorf("max %d: got %q, want %q", tc.max, got, tc.want)
		}
	}
	// No-op cases.
	if got := RuneSafeTailStart(data, len(data)); got != 0 {
		t.Errorf("max == len: got %d, want 0", got)
	}
	if got := RuneSafeTailStart(data, 100); got != 0 {
		t.Errorf("max > len: got %d, want 0", got)
	}
	if got := RuneSafeTailStart(data, 0); got != 0 {
		t.Errorf("max 0 (no cap): got %d, want 0", got)
	}
}

// TestTruncateVariants verifies the Truncate option matrix: the marker is
// appended OUTSIDE the budget by default (result may exceed max by
// len(marker)); MarkerInBudget reserves room for the marker (result is at
// most max, marker omitted when it would not fit); ForceMarker marks input
// that fits but was cut upstream; and fitting input passes through
// unmarked by default.
func TestTruncateVariants(t *testing.T) {
	marker := "\n… truncated (20 bytes total)"
	long := strings.Repeat("0123456789", 20) // 200 bytes
	multibyte := strings.Repeat("日本語のテキスト結果が長すぎる", 3)

	t.Run("marker outside budget by default", func(t *testing.T) {
		got := Truncate(long, 100, TruncateOptions{Marker: marker})
		if want := 100 + len(marker); len(got) != want {
			t.Fatalf("got %d bytes, want %d (100 cut + marker)", len(got), want)
		}
		if !strings.HasSuffix(got, marker) {
			t.Fatalf("expected marker suffix, got %q", got)
		}
	})

	t.Run("MarkerInBudget keeps result within max", func(t *testing.T) {
		got := Truncate(long, 100, TruncateOptions{Marker: marker, MarkerInBudget: true})
		if len(got) > 100 {
			t.Fatalf("capped result exceeds max: %d bytes", len(got))
		}
		if !strings.HasSuffix(got, marker) {
			t.Fatalf("expected marker suffix, got %q", got)
		}
		if got[:len(got)-len(marker)] != long[:100-len(marker)] {
			t.Fatalf("expected prefix of the original, got %q", got)
		}
	})

	t.Run("MarkerInBudget omits marker when it does not fit", func(t *testing.T) {
		got := Truncate(long, 10, TruncateOptions{Marker: marker, MarkerInBudget: true})
		if got != "0123456789" {
			t.Fatalf("expected exact-cap cut without marker, got %q", got)
		}
	})

	t.Run("fitting input passes through unmarked by default", func(t *testing.T) {
		if got := Truncate("abc", 10, TruncateOptions{Marker: marker}); got != "abc" {
			t.Fatalf("marker outside budget: got %q, want unchanged", got)
		}
		if got := Truncate("abc", 10, TruncateOptions{Marker: marker, MarkerInBudget: true}); got != "abc" {
			t.Fatalf("MarkerInBudget: got %q, want unchanged", got)
		}
		if got := Truncate("abc", 0, TruncateOptions{Marker: marker}); got != "abc" {
			t.Fatalf("max 0: got %q, want unchanged", got)
		}
	})

	t.Run("exact-cap input: pass-through vs known-cut", func(t *testing.T) {
		atCap := strings.Repeat("0123456789", 10) // exactly 100 bytes
		// Without ForceMarker the input fits, so it passes through
		// unmarked (no truncation happened).
		if got := Truncate(atCap, 100, TruncateOptions{Marker: marker, MarkerInBudget: true}); got != atCap {
			t.Fatalf("fitting input must pass through, got %q", got)
		}
		// With ForceMarker the input was cut upstream, so the marker is
		// appended even at the exact cap (regression: the bounded
		// command-output writer delivers content at exactly the cap).
		got := Truncate(atCap, 100, TruncateOptions{Marker: marker, MarkerInBudget: true, ForceMarker: true})
		if len(got) > 100 {
			t.Fatalf("ForceMarker: result exceeds max: %d bytes", len(got))
		}
		if !strings.HasSuffix(got, marker) {
			t.Fatalf("ForceMarker: expected marker suffix, got %q", got)
		}
	})

	t.Run("all option combinations keep valid UTF-8 at rune boundaries", func(t *testing.T) {
		for name, got := range map[string]string{
			"marker-outside":       Truncate(multibyte, 100, TruncateOptions{Marker: marker}),
			"marker-inside":        Truncate(multibyte, 100, TruncateOptions{Marker: marker, MarkerInBudget: true}),
			"marker-inside-tight":  Truncate(multibyte, 10, TruncateOptions{Marker: marker, MarkerInBudget: true}),
			"force-marker-inside":  Truncate(multibyte, 100, TruncateOptions{Marker: marker, MarkerInBudget: true, ForceMarker: true}),
			"force-marker-outside": Truncate(multibyte, 100, TruncateOptions{Marker: marker, ForceMarker: true}),
			"no-marker":            Truncate(multibyte, 100, TruncateOptions{}),
			"force-no-marker":      Truncate(multibyte, 100, TruncateOptions{ForceMarker: true}),
		} {
			if !utf8.ValidString(got) {
				t.Fatalf("%s: result is not valid UTF-8: %q", name, got)
			}
		}
	})
}
