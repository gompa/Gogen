package llm

import (
	"bytes"
	"encoding/json"
	"testing"
)

// These tests verify the byte-prefix stability of the serialized message
// list, which is what provider-side prompt caches (Anthropic, DeepSeek,
// OpenAI) key on: they cache the longest common prefix of the request, so a
// request whose leading messages serialize identically to a previous request
// gets cache hits, and any divergence point marks where the cache prefix
// breaks. The actual cache hit/miss numbers live on the provider and cannot
// be asserted here — but the property that *determines* them can.
//
// The per-message JSON is what messagesToChat produces for the wire,
// including reasoning_content extra fields and pinned ArgsStr, so these
// tests would catch any change that perturbs earlier messages on append,
// fork, or history edit.

// serializeMessages returns one canonical wire-JSON blob per message, in
// order, exactly as the provider request would contain them.
func serializeMessages(msgs []Message) [][]byte {
	p := &OpenAIProvider{}
	chat := p.messagesToChat(msgs)
	out := make([][]byte, len(chat))
	for i, c := range chat {
		b, err := json.Marshal(c)
		if err != nil {
			panic(err) // test-only
		}
		out[i] = b
	}
	return out
}

// commonPrefixLen returns the number of leading messages whose wire bytes are
// identical between two serialized histories.
func commonPrefixLen(a, b [][]byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if !bytes.Equal(a[i], b[i]) {
			return i
		}
	}
	return n
}

// TestNormalProgressionPreservesSerializedPrefix verifies that appending new
// turns never perturbs the wire bytes of earlier messages: request N+1 must
// begin with request N's exact bytes, or the provider cache breaks for the
// whole conversation on every new turn.
func TestNormalProgressionPreservesSerializedPrefix(t *testing.T) {
	base := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1", Reasoning: "think 1"},
	}
	extended := append(append([]Message(nil), base...),
		Message{Role: "user", Content: "q2"},
		Message{Role: "assistant", Content: "a2", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", ArgsStr: `{"path":"a.go"}`}}},
		Message{Role: "tool", Content: "file", ToolCallID: "c1"},
	)

	a, b := serializeMessages(base), serializeMessages(extended)
	if got := commonPrefixLen(a, b); got != len(a) {
		t.Fatalf("earlier messages changed on append: prefix preserved through %d of %d", got, len(a))
	}
}

// TestForkedHistoryIsBytePrefix verifies a fork that copies a prefix without
// editing (no tool-call strip, no ghost removal) serializes byte-identical to
// the original at those positions, so the provider cache covers the whole
// forked history on the first request after the fork.
func TestForkedHistoryIsBytePrefix(t *testing.T) {
	full := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
	}
	fork := full[:3] // fork at index 2 (the assistant reply)

	a, b := serializeMessages(full), serializeMessages(fork)
	if got := commonPrefixLen(a, b); got != len(b) {
		t.Fatalf("forked history diverges at message %d, want full prefix (%d)", got, len(b))
	}
}

// TestForkToolCallStripDivergesOnlyAtForkPoint verifies the cost of the
// mandatory tool-call strip at the fork point: everything before it stays
// byte-identical, and only the fork-point message itself differs. The cache
// bust is bounded to that single message.
func TestForkToolCallStripDivergesOnlyAtForkPoint(t *testing.T) {
	before := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
	}
	forkPoint := Message{Role: "assistant", Content: "let me check", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", ArgsStr: `{"path":"a.go"}`}}}
	full := append(append([]Message(nil), before...), forkPoint, Message{Role: "tool", Content: "file", ToolCallID: "c1"})
	stripped := append(append([]Message(nil), before...), Message{Role: "assistant", Content: "let me check"})

	a, b := serializeMessages(full), serializeMessages(stripped)
	if got := commonPrefixLen(a, b); got != len(before) {
		t.Fatalf("divergence at message %d, want %d (everything before the fork point identical)", got, len(before))
	}
}

// TestMidHistoryRemovalDivergesAtRemovalPoint quantifies why stripping a
// ghost from the middle of history is cache-hostile: the byte prefix breaks
// at the removal index, so every message after the removed ghost loses the
// provider cache even though the fork itself preserved them verbatim.
func TestMidHistoryRemovalDivergesAtRemovalPoint(t *testing.T) {
	full := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Reasoning: "truncated thinking"}, // ghost at index 2
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	without := []Message{full[0], full[1], full[3], full[4]}

	a, b := serializeMessages(full), serializeMessages(without)
	got := commonPrefixLen(a, b)
	if got != 2 {
		t.Fatalf("divergence at message %d, want 2 (the removal index)", got)
	}
	lost := len(full) - 1 - got
	if lost <= 0 {
		t.Fatalf("expected a non-empty tail to lose its cache prefix")
	}
}
