package streambuf

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestUTF16Len(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "hello", 5},
		{"cjk one unit per rune", "中文", 2},
		{"emoji two units", "😀", 2},
		{"mixed", "a中😀", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UTF16Len(tt.in); got != tt.want {
				t.Fatalf("UTF16Len(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// The empty buffer IS the "between rounds" marker: Snapshot returns nil
// before the first token and after every Reset, so a join between rounds
// never re-renders committed content.
func TestSnapshotNilWhenEmpty(t *testing.T) {
	var b RoundBuffer
	if s := b.Snapshot(); s != nil {
		t.Fatalf("fresh buffer: Snapshot = %+v, want nil", s)
	}
	b.AppendContent("x")
	b.Reset()
	if s := b.Snapshot(); s != nil {
		t.Fatalf("after reset: Snapshot = %+v, want nil", s)
	}
	// Empty appends are no-ops and must not make the buffer non-empty.
	b.AppendContent("")
	b.AppendThinking("")
	b.AppendToolArgs(0, "")
	if s := b.Snapshot(); s != nil {
		t.Fatalf("after empty appends: Snapshot = %+v, want nil", s)
	}
}

func TestAppendAndSnapshot(t *testing.T) {
	var b RoundBuffer
	b.AppendThinking("let me think")
	b.AppendContent("partial re")

	s := b.Snapshot()
	if s == nil {
		t.Fatal("Snapshot = nil, want non-nil")
	}
	if s.Thinking != "let me think" || s.Content != "partial re" {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.ThinkingPos != UTF16Len("let me think") || s.ContentPos != UTF16Len("partial re") {
		t.Fatalf("positions = %d/%d, want %d/%d", s.ThinkingPos, s.ContentPos,
			UTF16Len("let me think"), UTF16Len("partial re"))
	}
	if len(s.ToolCalls) != 0 {
		t.Fatalf("ToolCalls = %+v, want none", s.ToolCalls)
	}
}

// Positions are UTF-16 code units (not bytes): a multi-byte body must
// report the same unit count the client's trim will compute.
func TestPositionsAreUTF16Units(t *testing.T) {
	var b RoundBuffer
	body := strings.Repeat("línea con acento y emoji 😀\n", 10)
	b.AppendContent(body)
	b.AppendThinking("中文")

	s := b.Snapshot()
	if s.ContentPos != UTF16Len(body) {
		t.Fatalf("ContentPos = %d, want %d (UTF-16 units, not bytes)", s.ContentPos, UTF16Len(body))
	}
	if s.ThinkingPos != 2 {
		t.Fatalf("ThinkingPos = %d, want 2", s.ThinkingPos)
	}
}

// Tool calls: start + args deltas accumulate per index, and the snapshot
// lists them sorted by index (map iteration order must not leak out).
func TestToolCallsAccumulateAndSort(t *testing.T) {
	var b RoundBuffer
	// Insert out of order on purpose.
	b.ToolStart(2, "call_3", "read_file")
	b.AppendToolArgs(2, `{"path": "a.go"}`)
	b.ToolStart(0, "call_1", "execute_command")
	b.AppendToolArgs(0, `{"command": "ls"}`)
	b.ToolStart(1, "call_2", "patch_file")
	b.AppendToolArgs(1, `{"diff": "part1"}`)
	b.AppendToolArgs(1, `part2`)

	s := b.Snapshot()
	if s == nil || len(s.ToolCalls) != 3 {
		t.Fatalf("snapshot = %+v, want 3 tool calls", s)
	}
	for i, tc := range s.ToolCalls {
		if tc.Index != i {
			t.Fatalf("ToolCalls[%d].Index = %d, want %d (must be sorted)", i, tc.Index, i)
		}
	}
	if s.ToolCalls[0].Name != "execute_command" || s.ToolCalls[0].ID != "call_1" ||
		s.ToolCalls[0].Args != `{"command": "ls"}` {
		t.Fatalf("ToolCalls[0] = %+v", s.ToolCalls[0])
	}
	if s.ToolCalls[1].Args != `{"diff": "part1"}`+"part2" {
		t.Fatalf("ToolCalls[1].Args = %q (deltas must accumulate)", s.ToolCalls[1].Args)
	}
	if s.ToolCalls[1].ArgsPos != UTF16Len(`{"diff": "part1"}`+"part2") {
		t.Fatalf("ToolCalls[1].ArgsPos = %d, want %d", s.ToolCalls[1].ArgsPos,
			UTF16Len(`{"diff": "part1"}`+"part2"))
	}
}

// A fresh call at a recycled index restarts its args (and position) from
// zero — the old call's accumulation must not leak into the new one.
func TestToolStartRecycledIndex(t *testing.T) {
	var b RoundBuffer
	b.ToolStart(0, "call_1", "read_file")
	b.AppendToolArgs(0, `{"path": "old"}`)
	b.ToolStart(0, "call_2", "write_file")

	s := b.Snapshot()
	if s == nil || len(s.ToolCalls) != 1 {
		t.Fatalf("snapshot = %+v, want 1 tool call", s)
	}
	tc := s.ToolCalls[0]
	if tc.ID != "call_2" || tc.Name != "write_file" || tc.Args != "" || tc.ArgsPos != 0 {
		t.Fatalf("recycled index = %+v, want fresh call_2 with empty args", tc)
	}
}

// Args deltas arriving for an index with no ToolStart must not panic and
// must not make the snapshot non-empty (the snapshot iterates toolNames).
func TestArgsDeltaWithoutStart(t *testing.T) {
	var b RoundBuffer
	b.AppendToolArgs(0, `{"orphan": true}`)
	if s := b.Snapshot(); s != nil {
		t.Fatalf("snapshot = %+v, want nil (no tool name recorded)", s)
	}
	// A later ToolStart at the same index starts the args fresh (the
	// builder is replaced — the normal flow always starts before the
	// first delta, so orphans are hypothetical).
	b.ToolStart(0, "call_1", "read_file")
	s := b.Snapshot()
	if s == nil || len(s.ToolCalls) != 1 || s.ToolCalls[0].Args != "" {
		t.Fatalf("snapshot = %+v, want the call with fresh (empty) args", s)
	}
}

// Reset clears everything: builders, maps, and unit counters.
func TestResetClearsAll(t *testing.T) {
	var b RoundBuffer
	b.AppendThinking("think")
	b.AppendContent("abc")
	b.ToolStart(0, "call_1", "read_file")
	b.AppendToolArgs(0, `{"path": "a.go"}`)
	if s := b.Snapshot(); s == nil {
		t.Fatal("pre-reset snapshot = nil")
	}
	b.Reset()
	if s := b.Snapshot(); s != nil {
		t.Fatalf("post-reset snapshot = %+v, want nil", s)
	}
	// Double reset (turn start on an already-empty buffer) is a no-op.
	b.Reset()
	if s := b.Snapshot(); s != nil {
		t.Fatalf("second reset: snapshot = %+v, want nil", s)
	}
}

// Concurrent appends and snapshots must be race-free (run with -race):
// the stream goroutine appends while join goroutines snapshot.
func TestConcurrentAppendAndSnapshot(t *testing.T) {
	var b RoundBuffer
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				b.AppendThinking(fmt.Sprintf("t%d-%d", g, i))
				b.AppendContent(fmt.Sprintf("c%d-%d😀", g, i))
				b.ToolStart(0, "call", "read_file")
				b.AppendToolArgs(0, "{}")
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if s := b.Snapshot(); s != nil {
					// Invariant: positions always match the snapshot text
					// (atomic buffer+units under one lock).
					if s.ContentPos != UTF16Len(s.Content) {
						t.Errorf("ContentPos %d != UTF16Len(Content) %d", s.ContentPos, UTF16Len(s.Content))
						return
					}
					if s.ThinkingPos != UTF16Len(s.Thinking) {
						t.Errorf("ThinkingPos %d != UTF16Len(Thinking) %d", s.ThinkingPos, UTF16Len(s.Thinking))
						return
					}
					for _, tc := range s.ToolCalls {
						if tc.ArgsPos != UTF16Len(tc.Args) {
							t.Errorf("ToolCall[%d].ArgsPos %d != UTF16Len(Args) %d", tc.Index, tc.ArgsPos, UTF16Len(tc.Args))
							return
						}
					}
				}
			}
		}()
	}
	wg.Wait()
}
