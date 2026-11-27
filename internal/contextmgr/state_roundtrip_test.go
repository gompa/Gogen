package contextmgr

import (
	"fmt"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestStateKeysSurviveTheManifestUnchanged pins the lossless half of the key
// contract for OPERATION and FILE keys — the same property the handle test
// below pins, and the half that used to be violated upstream: argStateText
// folded whitespace and cut at 100 runes, so a >100-rune path re-imported with
// an ellipsis in it (a path that does not exist) and a command with a double
// space re-imported as a DIFFERENT key, splitting one ledger entry into two and
// making a file's history look unrelated to its earlier edit.
//
// The fix is not a bigger cap: single-line rendering is quoteState's job
// (strconv.Quote escapes newlines and quotes) and FITTING the manifest is
// renderState's job (an over-long line is omitted and COUNTED, never
// rewritten) — verified in the sub-test below.
func TestStateKeysSurviveTheManifestUnchanged(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	// Comfortably past the old 100-rune cap.
	longPath := "internal/" + strings.Repeat("d", 60) + "/" + strings.Repeat("f", 60) + ".go"
	if n := len([]rune(longPath)); n <= 100 {
		t.Fatalf("test path is %d runes, want > 100", n)
	}
	// A double space is meaningful inside a quoted -tags value; folding it
	// changes what the command does, so it changes the entry's identity.
	doubleSpaceCmd := `go build -tags "a  b" ./...`
	// A multi-line command needs no folding either: quoteState escapes the
	// newlines, so the line stays one line on the wire and unquotes back to the
	// same string.
	multiLineCmd := "make build\n  make test"

	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": longPath}},
			{ID: "c2", Name: "execute_command", Args: map[string]any{"command": doubleSpaceCmd}},
			{ID: "c3", Name: "execute_command", Args: map[string]any{"command": multiLineCmd}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "package main"},
		{Role: "tool", ToolCallID: "c2", Content: "ok"},
		{Role: "tool", ToolCallID: "c3", Content: "ok"},
	})

	block := m.renderState(st, maxStateTokens)
	if block == "" {
		t.Fatal("renderState returned an empty manifest")
	}
	restored := NewSessionState()
	restored.importBlock(block)

	if f := restored.files[longPath]; f == nil {
		t.Fatalf("restored manifest lost the long file key %q; block:\n%s", longPath, block)
	}
	for _, key := range []struct{ tool, arg string }{
		{"read_file", longPath},
		{"execute_command", doubleSpaceCmd},
		{"execute_command", multiLineCmd},
	} {
		if op := restored.operations[key.tool+"\x00"+key.arg]; op == nil {
			t.Fatalf("restored manifest lost the %s key %q; block:\n%s", key.tool, key.arg, block)
		}
	}
	// Nothing came back under a rewritten spelling (a folded or truncated
	// key would show up as an extra entry, not as a missing one).
	for path := range restored.files {
		if path != longPath {
			t.Fatalf("restored an unexpected file key %q (only %q was folded); block:\n%s", path, longPath, block)
		}
	}
	for id := range restored.operations {
		if strings.Contains(id, "\x00read_file\x00") && !strings.HasSuffix(id, "\x00"+longPath) {
			t.Fatalf("restored operation key %q is a rewritten %q", id, longPath)
		}
	}

	t.Run("an over-long entry line is omitted, never corrupted", func(t *testing.T) {
		huge := "internal/" + strings.Repeat("p", 4000) + ".go"
		st := NewSessionState()
		st.Fold([]llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: "c1", Name: "write_file", Args: map[string]any{"path": huge, "content": "x"},
			}}},
			{Role: "tool", ToolCallID: "c1", Content: "File written successfully"},
		})
		block := m.renderState(st, maxStateTokens)
		if block == "" {
			t.Fatal("renderState returned an empty manifest")
		}
		// The honest outcome: the line does not fit the budget, so it is
		// dropped and the omission note counts it. The old code instead
		// rendered the key 100 runes long plus "…" — an entry for a path that
		// does not exist, with no indication that anything was cut.
		if !strings.Contains(block, "state entries omitted") {
			t.Fatalf("the over-long line was neither rendered nor counted:\n%s", block)
		}
		restored := NewSessionState()
		restored.importBlock(block)
		for path := range restored.files {
			t.Fatalf("restored a key that was never folded: %q", path)
		}
		if strings.Contains(block, "…") && !strings.Contains(block, "state entries omitted") {
			t.Fatalf("the manifest carries an ellipsis-only key:\n%s", block)
		}
	})
}

// TestStateLongHandleKeyRoundTrips pins the manifest's key format: the block is
// parsed back on restore/fork and a handle key is an ADDRESS the model is told
// to reuse (a spill locator goes straight into read_file). quoteState used to
// fold whitespace and cut the key at 100 runes, so a long locator path came
// back with an ellipsis in it — a path that does not exist.
func TestStateLongHandleKeyRoundTrips(t *testing.T) {
	// Comfortably past the old 100-rune cap, and with a space in it: both
	// truncation and whitespace folding would corrupt it.
	longPath := "/home/user/workspace/project with spaces/.gogen/spill/" +
		strings.Repeat("s", 40) + "/" + strings.Repeat("o", 40) + "-output.log"
	if n := len([]rune(longPath)); n <= 100 {
		t.Fatalf("test path is %d runes, want > 100", n)
	}
	tests := []struct {
		name string
		key  string
	}{
		{"long path", longPath},
		{"path with a space", "/tmp/my dir/spill.log"},
		{"path with a quote", `/tmp/it's "quoted".log`},
		{"short path", "/tmp/short.log"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, DefaultSettings())
			st := NewSessionState()
			result := fmt.Sprintf("… total; full output saved to %s — use read_file offset/limit", tc.key)
			st.Fold([]llm.Message{
				{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "execute_command", Args: map[string]any{"command": "make"},
				}}},
				{Role: "tool", ToolCallID: "c1", Content: result},
			})
			if h := st.handles[stateKindSpill+"\x00"+tc.key]; h == nil {
				t.Fatalf("setup: spill handle %q not folded", tc.key)
			}

			block := m.renderState(st, maxStateTokens)
			if block == "" {
				t.Fatal("renderState returned an empty manifest")
			}
			restored := NewSessionState()
			restored.importBlock(block)
			h := restored.handles[stateKindSpill+"\x00"+tc.key]
			if h == nil {
				t.Fatalf("restored manifest lost the handle key %q; block:\n%s", tc.key, block)
			}
			if len(restored.handles) != 1 {
				t.Fatalf("restored %d handles, want only the original key; block:\n%s", len(restored.handles), block)
			}
			if h.Status != stateStatusRecoverable {
				t.Fatalf("restored status = %q, want recoverable", h.Status)
			}
		})
	}
}
