package llm

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go"
)

func deltaTool(index int64, id, name, args string) openai.ChatCompletionChunkChoiceDeltaToolCall {
	return openai.ChatCompletionChunkChoiceDeltaToolCall{
		Index: index,
		ID:    id,
		Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
			Name:      name,
			Arguments: args,
		},
	}
}

// deltaToolFromJSON builds a tool-call delta from its raw wire JSON so the
// JSON.* presence metadata — which the SSE decoder populates and the index
// routing relies on to distinguish an omitted index from an explicit index=0
// — is set exactly as it would be in a real stream.
func deltaToolFromJSON(t *testing.T, raw string) openai.ChatCompletionChunkChoiceDeltaToolCall {
	t.Helper()
	var tc openai.ChatCompletionChunkChoiceDeltaToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatal(err)
	}
	return tc
}

func TestMergeToolCallDeltaMultipleTools(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"a"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "", "", `{"path":"b"}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("got %d accums", len(accums))
	}
	if accums[0].ArgsStr != `{"path":"a"}` || accums[1].ArgsStr != `{"path":"b"}` {
		t.Fatalf("args = %#v", accums)
	}
}

func TestParseToolCallArgs(t *testing.T) {
	t.Parallel()
	_, err := parseToolCallArgs("{")
	if err == nil {
		t.Fatal("expected error for incomplete JSON")
	}
	_, err = parseToolCallArgs(`{"path":"x"`)
	if err == nil {
		t.Fatal("expected error for truncated JSON")
	}
	args, err := parseToolCallArgs(`{"path":"x"}`)
	if err != nil || args["path"] != "x" {
		t.Fatalf("got %#v err=%v", args, err)
	}
	args, err = parseToolCallArgs("")
	if err != nil || len(args) != 0 {
		t.Fatalf("empty args: %#v err=%v", args, err)
	}
}

func TestToolAccumsStreamComplete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		accums []tcAccum
		want   bool
	}{
		{"empty", nil, false},
		{"name only incomplete brace", []tcAccum{{Name: "read_file", ArgsStr: "{"}}, false},
		{"name only empty args", []tcAccum{{Name: "read_file", ArgsStr: ""}}, false},
		{"complete empty object", []tcAccum{{Name: "read_file", ArgsStr: "{}"}}, true},
		{"complete json", []tcAccum{{Name: "read_file", ArgsStr: `{"path":"a"}`}}, true},
		{"partial json", []tcAccum{{Name: "read_file", ArgsStr: `{"path":`}}, false},
		{"two tools one partial", []tcAccum{
			{Name: "read_file", ArgsStr: `{}`},
			{Name: "glob", ArgsStr: `{"pattern":"`},
		}, false},
		{"two tools complete", []tcAccum{
			{Name: "read_file", ArgsStr: `{}`},
			{Name: "glob", ArgsStr: `{"pattern":"*.go"}`},
		}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := toolAccumsStreamComplete(tc.accums); got != tc.want {
				t.Fatalf("toolAccumsStreamComplete() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeltaIsTerminalToolSignal(t *testing.T) {
	t.Parallel()
	var delta openai.ChatCompletionChunkChoiceDelta
	if deltaIsTerminalToolSignal(delta, false) {
		t.Fatal("no tools yet")
	}
	if err := delta.UnmarshalJSON([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !deltaIsTerminalToolSignal(delta, true) {
		t.Fatal("expected {} to end tool stream")
	}
}

func TestMergeToolCallDeltaArgsContinuationMissingIndex(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"a"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	// llama.cpp-style: continuation chunk defaults index to 0.
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"b"}`), accums, m)

	if accums[1].ArgsStr != `{"path":"b"}` {
		t.Fatalf("second tool args = %q, want b path", accums[1].ArgsStr)
	}
	if accums[0].ArgsStr != `{"path":"a"}` {
		t.Fatalf("first tool polluted: %q", accums[0].ArgsStr)
	}
}

// TestMergeToolCallDeltaExplicitIndexZeroContinuation pins the routing of an
// EXPLICIT index=0 args-only continuation while a higher-index tool is
// streaming. The default-index-0 splice must only apply when the wire OMITTED
// the index field; an explicit index=0 is a genuine continuation of tool 0
// and must land on tool 0, not on the last (highest-index) tool.
func TestMergeToolCallDeltaExplicitIndexZeroContinuation(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"a"`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "", "", `{"path":"b"}`), accums, m)
	// Explicit index=0 continuation carrying a true delta fragment: the
	// index field is PRESENT on the wire, so the fragment must continue
	// tool 0 (the pre-fix code spliced it onto tool 1 because 1 > 0).
	accums, _ = mergeToolCallDelta(deltaToolFromJSON(t, `{"index":0,"function":{"arguments":",\"offset\":10}"}}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("got %d accums, want 2", len(accums))
	}
	if accums[0].ArgsStr != `{"path":"a","offset":10}` {
		t.Fatalf("tool 0 args = %q, want explicit index-0 continuation spliced onto tool 0", accums[0].ArgsStr)
	}
	if accums[1].ArgsStr != `{"path":"b"}` {
		t.Fatalf("tool 1 polluted: %q", accums[1].ArgsStr)
	}
}

// TestMergeToolCallDeltaExplicitHighIndexContinuation verifies an explicit
// non-zero index routes through the index map (not the default-index-0
// splice), so an interleaved continuation of tool 2 is not spliced onto the
// last tool.
func TestMergeToolCallDeltaExplicitHighIndexContinuation(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(2, "c", "glob", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(2, "", "", `{"pattern":"*.go"`), accums, m)
	// Explicit index=2 continuation: must land on tool 2 via the map.
	accums, _ = mergeToolCallDelta(deltaToolFromJSON(t, `{"index":2,"function":{"arguments":",\"recursive\":true}"}}`), accums, m)

	if accums[2].ArgsStr != `{"pattern":"*.go","recursive":true}` {
		t.Fatalf("tool 2 args = %q, want explicit index-2 continuation", accums[2].ArgsStr)
	}
	if accums[0].ArgsStr != "" || accums[1].ArgsStr != "" {
		t.Fatalf("tools 0/1 polluted: %#v", accums)
	}
}

func TestMergeToolArgsDeltaCumulativeAndReplay(t *testing.T) {
	t.Parallel()

	// Cumulative snapshots (not deltas): each chunk re-sends from the start.
	got := mergeToolArgsDelta(`{"path"`, `{"path":"x.go"}`)
	if got != `{"path":"x.go"}` {
		t.Fatalf("cumulative: got %q", got)
	}

	// Full-object replay after complete JSON → "invalid character '{' after top-level value"
	got = mergeToolArgsDelta(`{"pattern":"foo"}`, `{"pattern":"foo"}`)
	if got != `{"pattern":"foo"}` {
		t.Fatalf("replay: got %q", got)
	}

	// True delta still concatenates.
	got = mergeToolArgsDelta(`{"path":"`, `x.go"}`)
	if got != `{"path":"x.go"}` {
		t.Fatalf("delta: got %q", got)
	}
}

func TestMergeToolCallDeltaNewIDReusesIndex(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "search_code", `{"pattern":"foo"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "b", "read_file", `{"path":"x.go"}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("got %d accums, want 2", len(accums))
	}
	if accums[0].ArgsStr != `{"pattern":"foo"}` {
		t.Fatalf("first polluted: %q", accums[0].ArgsStr)
	}
	if accums[1].Name != "read_file" || accums[1].ArgsStr != `{"path":"x.go"}` {
		t.Fatalf("second = %#v", accums[1])
	}
}

func TestParseToolCallArgsRecoversDuplicatedJSON(t *testing.T) {
	t.Parallel()
	args, err := parseToolCallArgs(`{"path":"x.go"}{"path":"x.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if args["path"] != "x.go" {
		t.Fatalf("got %#v", args)
	}
}

// TestMergeToolCallDeltaFinalizesOncePerAccumulator verifies that streaming
// incremental argument fragments marks the accumulator finalized exactly once
// the JSON closes, and that subsequent identical fragments do not re-run the
// JSON validity check (the argsFinalized cache stays set).
func TestMergeToolCallDeltaFinalizesOncePerAccumulator(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	// Open the tool call, then stream `{"path":"x.go"}` one fragment at a time.
	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	for _, frag := range []string{`{"path`, `":"x.go`, `"}`} {
		accums, _ = mergeToolCallDelta(deltaTool(0, "", "", frag), accums, m)
	}
	if len(accums) != 1 {
		t.Fatalf("got %d accums", len(accums))
	}
	if accums[0].ArgsStr != `{"path":"x.go"}` {
		t.Fatalf("args = %q", accums[0].ArgsStr)
	}
	if !accums[0].argsFinalized {
		t.Fatal("expected argsFinalized=true after complete JSON streamed")
	}

	// A subsequent replay fragment must keep the flag set (no re-validation).
	before := accums[0].argsFinalized
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"x.go"}`), accums, m)
	// Replay is ignored by mergeToolArgsDelta, so the buffer is unchanged and
	// the flag remains.
	if !accums[0].argsFinalized || accums[0].argsFinalized != before {
		t.Fatalf("replay should keep finalized flag set; got %v", accums[0].argsFinalized)
	}

	// toolAccumsStreamComplete should report true via the cached flag.
	if !toolAccumsStreamComplete(accums) {
		t.Fatal("expected toolAccumsStreamComplete true for finalized accumulator")
	}
}

// TestCompleteJSONObject covers the cheap structural pre-check used to decide
// when the expensive json.Unmarshal validity check is worth running. The
// check is string-aware: braces inside quoted strings (including escaped
// quotes) must not count toward nesting depth.
func TestCompleteJSONObject(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":           false,
		`{`:          false,
		`}`:          false,
		`{}`:         true,
		`{"a":1}`:    true,
		`{"a":`:      false,
		`{}}`:        false,
		`{"a":"}"}`:  true, // brace inside a string must not confuse the scan
		`{"a":1}{"b`: false,
		`{"a":"\""}`: true, // escaped quote inside a string
	}
	for s, want := range cases {
		if got := completeJSONObject(s); got != want {
			t.Fatalf("completeJSONObject(%q) = %v, want %v", s, got, want)
		}
	}
}
