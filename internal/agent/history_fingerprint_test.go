package agent

// The conditional web attach compares a client's rendered transcript against
// HistoryFingerprint (epoch + last shipped index) instead of paying for the
// SnapshotMessages deep clone. These tests pin the fingerprint semantics the
// server's skip decision relies on:
//   - HistoryShips mirrors exactly which messages a history payload would
//     include (placeholders skipped, images-only user messages kept);
//   - the epoch is stable across appends (appendMessage never bumps it) and
//     moves on wholesale replacement, so fingerprint equality means "the
//     conversation lineage the client rendered is still current";
//   - the pair is read under one lock, so a caller can never pair an epoch
//     from before a reshape with a lastShipped index from after it.

import (
	"testing"

	"gogen/internal/llm"
)

func TestHistoryShips(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  llm.Message
		want bool
	}{
		{"user text", llm.Message{Role: "user", Content: "q"}, true},
		{"user image only", llm.Message{Role: "user", Images: []llm.ImageInput{{DataURL: "data:image/png;base64,"}}}, true},
		{"user empty", llm.Message{Role: "user"}, false},
		{"assistant content", llm.Message{Role: "assistant", Content: "a"}, true},
		{"assistant tool calls only", llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "f"}}}, true},
		{"assistant reasoning only", llm.Message{Role: "assistant", Reasoning: "th"}, true},
		{"assistant refusal only", llm.Message{Role: "assistant", Refusal: "no"}, true},
		{"assistant empty", llm.Message{Role: "assistant"}, false},
		{"tool result", llm.Message{Role: "tool", Content: "out", ToolCallID: "c1"}, true},
		{"tool id only", llm.Message{Role: "tool", ToolCallID: "c1"}, true},
		{"tool empty", llm.Message{Role: "tool"}, false},
		{"unknown role", llm.Message{Role: "system", Content: "s"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HistoryShips(tt.msg); got != tt.want {
				t.Fatalf("HistoryShips(%+v) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestHistoryFingerprint(t *testing.T) {
	t.Parallel()
	t.Run("empty conversation has no shipped index", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(t.TempDir())
		a := NewAgent(nil, exec, nil)
		epoch, last := a.HistoryFingerprint()
		if epoch != 0 {
			t.Fatalf("fresh agent epoch = %d, want 0", epoch)
		}
		if last != -1 {
			t.Fatalf("fresh agent lastShipped = %d, want -1", last)
		}
	})

	t.Run("appends advance the index without bumping the epoch", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(t.TempDir())
		a := NewAgent(nil, exec, nil)
		a.appendMessage(llm.Message{Role: "user", Content: "q"})
		a.appendMessage(llm.Message{Role: "assistant", Content: "a"})
		epoch, last := a.HistoryFingerprint()
		if epoch != 0 {
			t.Fatalf("epoch after appends = %d, want 0 (appends never reshape)", epoch)
		}
		if last != 1 {
			t.Fatalf("lastShipped after two messages = %d, want 1", last)
		}
	})

	t.Run("trailing placeholders are skipped", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(t.TempDir())
		a := NewAgent(nil, exec, nil)
		a.appendMessage(llm.Message{Role: "user", Content: "q"})
		a.appendMessage(llm.Message{Role: "assistant", Content: "a"})
		// A trailing empty placeholder must not move the last SHIPPED index
		// even though it grew the message list.
		a.appendMessage(llm.Message{Role: "assistant"})
		epoch, last := a.HistoryFingerprint()
		if epoch != 0 || last != 1 {
			t.Fatalf("fingerprint = (%d, %d), want (0, 1)", epoch, last)
		}
	})

	t.Run("wholesale replacement bumps the epoch and drops the index", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(t.TempDir())
		a := NewAgent(nil, exec, nil)
		a.appendMessage(llm.Message{Role: "user", Content: "q"})
		a.appendMessage(llm.Message{Role: "assistant", Content: "a"})
		a.replaceMessages(nil)
		epoch, last := a.HistoryFingerprint()
		if epoch != 1 {
			t.Fatalf("epoch after reset = %d, want 1", epoch)
		}
		if last != -1 {
			t.Fatalf("lastShipped after reset = %d, want -1", last)
		}
	})

	t.Run("restored messages ship at their new indexes", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(t.TempDir())
		a := NewAgent(nil, exec, nil)
		a.restoreMessages([]llm.Message{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		}, nil)
		epoch, last := a.HistoryFingerprint()
		if epoch != 1 {
			t.Fatalf("epoch after restore = %d, want 1 (restore is a reshape)", epoch)
		}
		if last != 2 {
			t.Fatalf("lastShipped after restore = %d, want 2", last)
		}
	})
}
