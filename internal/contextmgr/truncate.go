package contextmgr

import "unicode/utf8"

// TruncateRuneSafe cuts s to at most max bytes without splitting a UTF-8
// rune: it backs off over continuation bytes until it lands on a rune
// boundary. s is assumed valid UTF-8 (tool results that pass through JSON
// decoding always are); for invalid input the result is never worse than a
// raw byte cut. Shared by the context window capper and the web server's
// per-frame tool-result cap so both produce valid UTF-8 output.
func TruncateRuneSafe(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// RuneSafeTailStart returns the byte offset at which a tail capped at max
// bytes of data may start without splitting a UTF-8 rune: the raw cut point
// (len(data)-max) is advanced forward over continuation bytes until it lands
// on a rune boundary. A raw byte cut can split a multi-byte character at the
// start of the shown tail and inject invalid UTF-8 into the tool result —
// the tail-cut mirror of TruncateRuneSafe (which makes head cuts rune-safe).
// data is assumed valid UTF-8 (command output usually is; for invalid input
// the result is never worse than a raw byte cut). The []byte signature
// exists so callers backed by bytes.Buffer can pass the offset straight to
// Buffer.Next. Returns 0 when the data fits within max, or max <= 0 (no cap).
func RuneSafeTailStart(data []byte, max int) int {
	if max <= 0 || len(data) <= max {
		return 0
	}
	start := len(data) - max
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return start
}

// TruncateOptions configures Truncate. The zero value is a plain rune-safe
// head cut with no marker — identical to TruncateRuneSafe.
type TruncateOptions struct {
	// Marker is appended when a cut is made. Empty means no marker.
	Marker string
	// MarkerInBudget reserves len(Marker) bytes inside max so the result
	// is at most max bytes. When false (the default) the marker is
	// appended OUTSIDE the budget and the result may exceed max by
	// len(Marker) — use this when the cap is a display limit and a few
	// extra bytes are harmless (web frames, subagent reports).
	MarkerInBudget bool
	// ForceMarker appends the marker even when s already fits within max.
	// Set it when the caller KNOWS s was cut upstream (e.g. by a bounded
	// writer): without the marker the result would claim no truncation
	// happened. When false (the default) fitting input passes through
	// unchanged.
	ForceMarker bool
}

// Truncate caps s to at most max bytes with a rune-safe head cut, optionally
// appending a marker. It is the single truncation primitive of the package:
// every truncation contract is Truncate with different options.
//
//   - plain cut, no marker: Truncate(s, max, TruncateOptions{})
//   - marker outside the budget: TruncateOptions{Marker: m}
//   - marker inside the budget: TruncateOptions{Marker: m, MarkerInBudget: true}
//   - known-cut input (mark even when fitting): add ForceMarker: true
//
// max <= 0 means "no cap" and returns s unchanged. With MarkerInBudget, a
// marker that would not fit alongside any content is omitted rather than
// exceeding the cap (the result is then exactly max bytes). s is assumed
// valid UTF-8, like TruncateRuneSafe; for invalid input the result is never
// worse than a raw byte cut.
func Truncate(s string, max int, opts TruncateOptions) string {
	if max <= 0 {
		return s
	}
	if !opts.ForceMarker && len(s) <= max {
		return s
	}
	if opts.Marker == "" {
		return TruncateRuneSafe(s, max)
	}
	if opts.MarkerInBudget {
		if len(opts.Marker) >= max {
			return TruncateRuneSafe(s, max)
		}
		return TruncateRuneSafe(s, max-len(opts.Marker)) + opts.Marker
	}
	return TruncateRuneSafe(s, max) + opts.Marker
}
