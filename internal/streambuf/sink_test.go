package streambuf

import "testing"

// TestRoundSinkTiming pins the shared reset/append rule every host relies
// on: the buffer is cleared at turn start, round start, and round end, and
// fed on every token/tool event. A mid-turn join snapshot must therefore
// always carry exactly the current round's in-flight output.
func TestRoundSinkTiming(t *testing.T) {
	var buf RoundBuffer
	s := RoundSink{Buf: &buf}

	s.TurnBegin()
	s.Token("a")
	s.Thinking("h")
	s.ToolStart(0, "id1", "f")
	s.ToolArgs(0, `{"a":1}`)
	if snap := buf.Snapshot(); snap == nil || snap.Content != "a" || snap.Thinking != "h" || len(snap.ToolCalls) != 1 {
		t.Fatalf("after turn start + events, snapshot = %+v", snap)
	}

	// Round start clears the previous round (its content is committed).
	s.RoundBegin()
	if snap := buf.Snapshot(); snap != nil {
		t.Fatalf("after round start, snapshot = %+v, want nil", snap)
	}

	// Round end clears the round (its message is committed).
	s.Token("b")
	s.RoundEnd()
	if snap := buf.Snapshot(); snap != nil {
		t.Fatalf("after round end, snapshot = %+v, want nil", snap)
	}
}

// TestRoundSinkZeroIsNoop pins that a zero RoundSink (adapters built
// without a session) is a silent no-op rather than a panic.
func TestRoundSinkZeroIsNoop(t *testing.T) {
	var s RoundSink
	s.TurnBegin()
	s.RoundBegin()
	s.RoundEnd()
	s.Token("a")
	s.Thinking("h")
	s.ToolStart(0, "id", "f")
	s.ToolArgs(0, "x")
}

// TestRoundSinkHooks pins the host-extension contract: Reset replaces the
// buffer reset (the web server clears its sent markers with it), and
// OnToolStart runs AFTER the buffer's ToolStart for the same event.
func TestRoundSinkHooks(t *testing.T) {
	var buf RoundBuffer
	var resets, hookCalls int
	var hookSawBuffer bool
	s := RoundSink{
		Buf: &buf,
		Reset: func() {
			resets++
			buf.Reset()
		},
		OnToolStart: func(index int, id, name string) {
			hookCalls++
			// The buffer must already carry the tool call when the hook
			// runs (the sent-marker restart keys off the same index).
			hookSawBuffer = buf.Snapshot() != nil
		},
	}
	s.TurnBegin()
	s.RoundBegin()
	s.RoundEnd()
	if resets != 3 {
		t.Fatalf("Reset hook calls = %d, want 3", resets)
	}
	s.ToolStart(0, "id1", "f")
	if hookCalls != 1 || !hookSawBuffer {
		t.Fatalf("OnToolStart calls=%d sawBuffer=%v, want 1/true", hookCalls, hookSawBuffer)
	}
}
