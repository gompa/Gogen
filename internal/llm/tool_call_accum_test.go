package llm

import (
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
			{Name: "glob_files", ArgsStr: `{"pattern":"`},
		}, false},
		{"two tools complete", []tcAccum{
			{Name: "read_file", ArgsStr: `{}`},
			{Name: "glob_files", ArgsStr: `{"pattern":"*.go"}`},
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
