package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestStreamJSONScanMatchesUnmarshal feeds valid args documents one byte at
// a time (hitting every batch boundary) and asserts at each prefix that the
// incremental scanner's "top-level object closed" verdict equals the
// whole-buffer oracle — json.Unmarshal succeeding. That equivalence is what
// lets handleStreamToolArgs skip the per-batch unmarshal while the object
// is open without ever delaying a line update.
func TestStreamJSONScanMatchesUnmarshal(t *testing.T) {
	docs := map[string]string{
		"empty object":       `{}`,
		"single string key":  `{"a":"b"}`,
		"realistic patch":    `{"path":"f.go","dry_run":false,"diff":"--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+y\n"}`,
		"nested containers":  `{"a":{"b":[1,2,{"c":"}"}]},"d":true}`,
		"braces in strings":  `{"a":"brace } bracket ] in string"}`,
		"escapes in strings": `{"a":"quote \" backslash \\ tab \t end"}`,
		"mixed scalars":      `{"n":-1.5e3,"b":false,"z":null,"s":""}`,
		"whitespace":         `{ "a" : 1 , "b" : [ ] }`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			var s streamJSONScan
			buf := []byte(doc)
			for i := 1; i <= len(buf); i++ {
				prefix := buf[:i]
				s.advance(prefix)
				var oracle map[string]any
				want := json.Unmarshal(prefix, &oracle) == nil
				if s.closed != want {
					t.Fatalf("prefix %d (%q): closed=%v, unmarshal-ok=%v", i, prefix, s.closed, want)
				}
				if s.consumed > len(prefix) {
					t.Fatalf("prefix %d: consumed %d beyond buffer %d", i, s.consumed, len(prefix))
				}
			}
			// Closed is sticky: trailing bytes never un-close the object.
			s.advance(append(buf, ' ', ' '))
			if !s.closed {
				t.Fatal("closed not sticky after trailing bytes")
			}
		})
	}
}

// TestStreamJSONScanStaysClosedOnInvalid pins the once-only-parse
// guarantees on malformed input: an unterminated string never closes the
// scan, and once the root's closing brace is seen the buffer stays closed
// while never becoming parseable — so callers may cache the failed parse
// instead of re-attempting it per batch.
func TestStreamJSONScanStaysClosedOnInvalid(t *testing.T) {
	t.Run("unterminated string never closes", func(t *testing.T) {
		var s streamJSONScan
		buf := []byte(`{"a":"unterminated`)
		for i := 1; i <= len(buf); i++ {
			s.advance(buf[:i])
			if s.closed {
				t.Fatalf("prefix %d: closed inside unterminated string", i)
			}
		}
		s.advance(append(buf, 'x'))
		if s.closed {
			t.Fatal("later bytes un-closed an open string")
		}
	})
	t.Run("invalid contents stay unparseable after closure", func(t *testing.T) {
		var s streamJSONScan
		buf := []byte(`{"a":tru}`)
		s.advance(buf)
		if !s.closed {
			t.Fatal("scan did not close at the root's closing brace")
		}
		if _, err := parseInlineJSONArgs(buf); err == nil {
			t.Fatal("invalid JSON parsed")
		}
		s.advance(append(buf, ',', 'x'))
		if !s.closed {
			t.Fatal("closed not sticky for invalid contents")
		}
		if _, err := parseInlineJSONArgs(buf); err == nil {
			t.Fatal("append repaired unparseable JSON")
		}
	})
}

// TestStreamToolArgsGenericLineAtClosure streams a generic tool's args in
// small batches and asserts the tool-call line gains its formatted args
// exactly when the top-level object closes (never while it is open), and
// that the streaming line is already the final normalised line — so
// handleStreamToolCallFinal rewrites it with identical bytes.
func TestStreamToolArgsGenericLineAtClosure(t *testing.T) {
	t.Run("single key matches final form byte for byte", func(t *testing.T) {
		m := newStreamTestModel()
		m.handleStreamToolCall(0, "tc0", "write_file")
		lineIdx := m.streamToolCallLines[0]

		content := "package main\n\nfunc main() {}\n"
		b, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		raw := `{"content":` + string(b) + `}`
		for i := 0; i < len(raw); i += 5 {
			end := i + 5
			if end > len(raw) {
				end = len(raw)
			}
			m.handleStreamToolArgs(0, "tc0", raw[i:end])
			var oracle map[string]any
			if err := json.Unmarshal([]byte(raw[:end]), &oracle); err != nil {
				if got := stripAnsi(m.chatLines[lineIdx]); got != "  → write_file" {
					t.Fatalf("open batch end %d: line = %q, want plain", end, got)
				}
			}
		}
		streamed := stripAnsi(m.chatLines[lineIdx])
		if !strings.Contains(streamed, "content=") {
			t.Fatalf("closed line = %q, want formatted args", streamed)
		}
		m.handleStreamToolCallFinal(0, llm.ToolCall{Index: 0, ID: "tc0", Name: "write_file", Args: map[string]any{"content": content}})
		if after := stripAnsi(m.chatLines[lineIdx]); after != streamed {
			t.Fatalf("final line jumped: %q -> %q", streamed, after)
		}
	})

	t.Run("multi key args appear only at closure", func(t *testing.T) {
		m := newStreamTestModel()
		m.handleStreamToolCall(0, "tc0", "write_file")
		lineIdx := m.streamToolCallLines[0]

		raw := `{"path":"main.go","content":"x"}`
		for i := 0; i < len(raw); i += 3 {
			end := i + 3
			if end > len(raw) {
				end = len(raw)
			}
			m.handleStreamToolArgs(0, "tc0", raw[i:end])
			var oracle map[string]any
			if err := json.Unmarshal([]byte(raw[:end]), &oracle); err != nil {
				if got := stripAnsi(m.chatLines[lineIdx]); got != "  → write_file" {
					t.Fatalf("open batch end %d: line = %q, want plain", end, got)
				}
				continue
			}
			got := stripAnsi(m.chatLines[lineIdx])
			if !strings.Contains(got, `path="main.go"`) || !strings.Contains(got, `content="x"`) {
				t.Fatalf("closed batch end %d: line = %q, want both args", end, got)
			}
		}
	})
}

// TestPatchFileCompactLineAtClosure checks the patch_file args line: while
// the diff value streams the JSON is incomplete and the line stays plain;
// the compact non-diff args are computed once at object closure and stay
// stable afterwards.
func TestPatchFileCompactLineAtClosure(t *testing.T) {
	m := newStreamTestModel()
	m.width = 200
	m.handleStreamToolCall(0, "tc0", "patch_file")
	lineIdx := m.streamToolCallLines[0]

	diff, err := json.Marshal("--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+y\n")
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"path":"f.go","diff":` + string(diff) + `}`
	for i := 0; i < len(raw)-1; i += 4 {
		end := i + 4
		if end > len(raw)-1 {
			end = len(raw) - 1
		}
		m.handleStreamToolArgs(0, "tc0", raw[i:end])
		if got := stripAnsi(m.chatLines[lineIdx]); got != "  → patch_file" {
			t.Fatalf("open batch end %d: line = %q, want plain", end, got)
		}
	}
	// The closing brace completes the object; compact args appear once.
	m.handleStreamToolArgs(0, "tc0", raw[len(raw)-1:])
	assertDiffParity(t, m)
	got := stripAnsi(m.chatLines[lineIdx])
	if !strings.Contains(got, `path="f.go"`) || strings.Contains(got, "diff") {
		t.Fatalf("closed line = %q, want compact non-diff args only", got)
	}

	// Trailing batches must not change the finalized line.
	before := m.chatLines[lineIdx]
	m.handleStreamToolArgs(0, "tc0", " ")
	if m.chatLines[lineIdx] != before {
		t.Fatalf("trailing batch changed the finalized line: %q -> %q", before, m.chatLines[lineIdx])
	}
}

// TestStreamToolArgsScanStopsAtClosure pins the O(delta) structure on the
// Model state: the scanner walks each byte exactly once (consumed only
// advances, stopping at the closing brace instead of the buffer tail), and
// the derived lines are frozen after closure — no per-batch re-parse can
// change them.
func TestStreamToolArgsScanStopsAtClosure(t *testing.T) {
	t.Run("patch_file compact frozen after closure", func(t *testing.T) {
		m := newStreamTestModel()
		m.handleStreamToolCall(0, "tc0", "patch_file")

		raw := `{"diff":"` + strings.Repeat("x", 8192) + `"}`
		for i := 0; i < len(raw); i += 64 {
			end := i + 64
			if end > len(raw) {
				end = len(raw)
			}
			m.handleStreamToolArgs(0, "tc0", raw[i:end])
			ab := m.streamToolCallArgs[0]
			if !ab.scan.closed && ab.scan.consumed != len(ab.buf) {
				t.Fatalf("open batch end %d: consumed=%d buf=%d, scan lagging", end, ab.scan.consumed, len(ab.buf))
			}
		}
		ab := m.streamToolCallArgs[0]
		if !ab.scan.closed || ab.scan.consumed != len(ab.buf) {
			t.Fatalf("at closure: closed=%v consumed=%d buf=%d", ab.scan.closed, ab.scan.consumed, len(ab.buf))
		}
		if !ab.compactDone {
			t.Fatal("compact not finalized at closure")
		}
		compact, consumed := ab.compact, ab.scan.consumed
		m.handleStreamToolArgs(0, "tc0", " ")
		ab = m.streamToolCallArgs[0]
		if ab.compact != compact || ab.scan.consumed != consumed {
			t.Fatalf("trailing batch recomputed state: compact %q->%q consumed %d->%d",
				compact, ab.compact, consumed, ab.scan.consumed)
		}
	})

	t.Run("generic parse frozen after closure", func(t *testing.T) {
		m := newStreamTestModel()
		m.handleStreamToolCall(0, "tc0", "write_file")
		lineIdx := m.streamToolCallLines[0]

		raw := `{"path":"f.go"}`
		m.handleStreamToolArgs(0, "tc0", raw)
		ab := m.streamToolCallArgs[0]
		if !ab.scan.closed || !ab.genDone || ab.argStr == "" {
			t.Fatalf("closure state: closed=%v genDone=%v argStr=%q", ab.scan.closed, ab.genDone, ab.argStr)
		}
		line := m.chatLines[lineIdx]
		consumed := ab.scan.consumed
		m.handleStreamToolArgs(0, "tc0", " ")
		ab = m.streamToolCallArgs[0]
		if m.chatLines[lineIdx] != line || ab.scan.consumed != consumed {
			t.Fatalf("trailing batch changed finalized state: line %q -> %q, consumed %d -> %d",
				line, m.chatLines[lineIdx], consumed, ab.scan.consumed)
		}
	})
}

// BenchmarkStreamToolArgsBatchCost measures the arg-handling work of
// streaming a ~1 MiB patch_file diff in 4 KiB deltas: the O(delta) buffer
// append, completeness scan, and incremental diff extract per batch. The
// chat/viewport funnels are shared infrastructure and deliberately excluded
// (they dominate end-to-end batches and are unchanged by arg handling); the
// pre-fix path additionally re-unmarshaled the whole accumulated buffer —
// O(N), dominated by encoding/json's full-buffer validity pre-scan — on
// every batch.
func BenchmarkStreamToolArgsBatchCost(b *testing.B) {
	payload := strings.Repeat("+line of a large generated diff\n", 33000)
	raw := `{"path":"big.txt","diff":` + payloadJSON(payload) + `}`
	const batch = 4096
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := &Model{streamToolDiffRender: map[int]*streamToolDiffRender{}}
		ab := &streamToolArgs{}
		for j := 0; j < len(raw); j += batch {
			end := j + batch
			if end > len(raw) {
				end = len(raw)
			}
			ab.buf = append(ab.buf, raw[j:end]...)
			ab.scan.advance(ab.buf)
			m.advanceToolDiffRender(0, ab.buf)
		}
	}
}

// payloadJSON marshals payload as a JSON string, failing the benchmark on
// the (impossible) marshal error.
func payloadJSON(payload string) string {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(b)
}
