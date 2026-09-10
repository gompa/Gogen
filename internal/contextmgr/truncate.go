package contextmgr

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

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
// Buffer.Next; callers holding a string should use RuneSafeTailStartString
// instead, which avoids the []byte(s) allocation-and-copy. Returns 0 when
// the data fits within max, or max <= 0 (no cap).
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

// RuneSafeTailStartString is the string form of RuneSafeTailStart: it returns
// the byte offset at which a tail capped at max bytes of s may start without
// splitting a UTF-8 rune, with the same rune-boundary contract and the same
// 0 return when s fits within max (or max <= 0). Callers that already hold a
// string — the common case, since tool results are strings — use this to
// avoid the O(len) allocation-and-copy of []byte(s) that the []byte form
// would force; the returned offset is directly indexable as s[offset:].
func RuneSafeTailStartString(s string, max int) int {
	if max <= 0 || len(s) <= max {
		return 0
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
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

// TruncateHeadTailOptions configures TruncateHeadTail.
type TruncateHeadTailOptions struct {
	// HeadBytes is the byte budget for the kept head. The cut is
	// rune-safe (TruncateRuneSafe). 0 keeps no head.
	HeadBytes int
	// TailBytes is the byte budget for the kept tail. The cut is
	// rune-safe (RuneSafeTailStart). 0 keeps no tail.
	TailBytes int
	// Marker is inserted between the kept head and tail when a cut is
	// made. Empty joins head and tail directly. The marker is OUTSIDE the
	// head/tail budgets: the result may exceed HeadBytes+TailBytes by
	// len(marker).
	//
	// By default the marker passes through VERBATIM — a pre-rendered
	// marker's literal '%' (e.g. an embedded file path) is never touched
	// by Sprintf verb parsing. Only with FormatDropped is it treated as a
	// format string.
	Marker string
	// FormatDropped opts the marker into the dropped-bytes contract: when
	// set AND the marker contains a %d verb, it is Sprintf-formatted with
	// the number of DROPPED bytes (the middle actually removed, rune-safe
	// back-off included). A format-enabled marker without a %d verb still
	// passes through unchanged (no %!(EXTRA) tail).
	FormatDropped bool
	// ForceMarker appends the Marker after the FULL input even when no
	// cut is made (len(s) <= HeadBytes+TailBytes). Set it when the
	// caller knows the content was persisted beyond the kept parts and
	// the marker must appear regardless (spill previews do this: the
	// locator line must show even when the preview happens to hold
	// everything).
	ForceMarker bool
}

// TruncateHeadTail keeps the first HeadBytes and the last TailBytes of s,
// dropping the middle and inserting Marker between the two kept parts.
// Both cut points are rune-safe — a multi-byte rune is never split at
// either end of the result. It is the tail-preserving counterpart of
// Truncate: build/test failures put their error summary at the END of the
// output, which a head-only cut throws away.
//
// No-overlap guarantee: when len(s) <= HeadBytes+TailBytes the input fits
// both budgets and is returned unchanged (Marker appended when ForceMarker
// is set) — head and tail never duplicate or overlap bytes.
//
// dropped = len(s) - len(kept head) - len(kept tail) counts the middle
// bytes actually removed (rune-safe back-off included) and is the %d
// argument of a marker under the opt-in FormatDropped contract. A budget
// of 0 keeps nothing on that side; when both are 0 the result is just the
// marker. s is assumed valid UTF-8, like TruncateRuneSafe; for invalid
// input the result is never worse than a raw byte cut.
func TruncateHeadTail(s string, opts TruncateHeadTailOptions) string {
	if len(s) <= opts.HeadBytes+opts.TailBytes {
		if opts.ForceMarker && opts.Marker != "" {
			return s + formatDroppedMarker(opts.Marker, 0, opts.FormatDropped)
		}
		return s
	}
	var head, tail string
	if opts.HeadBytes > 0 {
		head = TruncateRuneSafe(s, opts.HeadBytes)
	}
	if opts.TailBytes > 0 {
		tail = s[RuneSafeTailStartString(s, opts.TailBytes):]
	}
	return head + formatDroppedMarker(opts.Marker, len(s)-len(head)-len(tail), opts.FormatDropped) + tail
}

// formatDroppedMarker applies the opt-in %d dropped-bytes contract: when
// format is set AND the marker contains a %d verb, it is Sprintf-formatted
// with dropped; any other marker passes through unchanged. The verb check
// keeps a format-enabled marker without %d from growing a %!(EXTRA) tail,
// and a non-format marker's literal '%' — e.g. a pre-rendered locator with
// an embedded file path — is never verb-parsed.
func formatDroppedMarker(marker string, dropped int, format bool) string {
	if !format || !strings.Contains(marker, "%d") {
		return marker
	}
	return fmt.Sprintf(marker, dropped)
}
