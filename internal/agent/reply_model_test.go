package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// lastAssistantMsg returns the most recent assistant message in msgs.
func lastAssistantMsg(msgs []llm.Message) (llm.Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i], true
		}
	}
	return llm.Message{}, false
}

// TestStreamResultModelStampedOnAssistantMessage verifies the
// provider-reported model (which may differ from the requested alias on
// router endpoints such as OpenCode Zen) is attached read-only to the
// assistant message, and that a turn without a reported model leaves the
// field empty.
func TestStreamResultModelStampedOnAssistantMessage(t *testing.T) {
	prov := llm.NewMockProvider()
	prov.StreamResults = []*llm.StreamResult{
		{Content: "answer", Model: "glm-4.6"},
	}
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	if _, err := a.StreamProcessInput(context.Background(), "hi", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	last, ok := lastAssistantMsg(a.Messages)
	if !ok {
		t.Fatal("no assistant message appended")
	}
	if last.Model != "glm-4.6" {
		t.Fatalf("assistant Model = %q, want %q", last.Model, "glm-4.6")
	}
	if last.Content != "answer" {
		t.Fatalf("assistant Content = %q, want %q", last.Content, "answer")
	}

	// A subsequent turn with no reported model must leave the field empty.
	prov.StreamResults = []*llm.StreamResult{{Content: "again"}}
	if _, err := a.StreamProcessInput(context.Background(), "again", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	last, ok = lastAssistantMsg(a.Messages)
	if !ok {
		t.Fatal("no assistant message appended")
	}
	if last.Model != "" {
		t.Fatalf("assistant Model = %q, want empty", last.Model)
	}
}

// TestStreamResultModelStampedOnToolCallRound verifies the model is stamped
// even when the turn's final round carries tool calls (the second StreamResult
// in a tool-call turn is the final assistant message).
func TestStreamResultModelStampedOnToolCallRound(t *testing.T) {
	prov := llm.NewMockProvider()
	prov.StreamResults = []*llm.StreamResult{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: map[string]any{"path": "x"}}}, Model: "glm-4.6"},
		{Content: "done", Model: "glm-4.6"},
	}
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	if _, err := a.StreamProcessInput(context.Background(), "read x", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	last, ok := lastAssistantMsg(a.Messages)
	if !ok {
		t.Fatal("no assistant message appended")
	}
	if last.Model != "glm-4.6" {
		t.Fatalf("assistant Model = %q, want %q", last.Model, "glm-4.6")
	}
}

// TestOnReplyModelFiredPerRoundBeforeStreamEnd verifies the live-model stamp
// is delivered for EVERY round of a turn — including intermediate rounds
// that carry content plus tool calls — and that each stamp precedes its
// round's stream_end so the client can apply it to the still-live bubble.
// (History replay attributes older rounds via the assistant message Model
// field, but the live bubble must not have to wait for that sync.)
func TestOnReplyModelFiredPerRoundBeforeStreamEnd(t *testing.T) {
	prov := llm.NewMockProvider()
	calls := 0
	prov.OnStream = func(_ context.Context, _ []llm.Message, h *llm.StreamHandlers) (*llm.StreamResult, error) {
		calls++
		if h.OnStreamOpened != nil {
			h.OnStreamOpened()
		}
		if calls == 1 {
			if h.OnToken != nil {
				h.OnToken("partial ")
			}
			return &llm.StreamResult{
				Content: "partial ",
				ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "read_file", Args: map[string]any{"path": "x"},
				}},
				Model: "glm-a",
			}, nil
		}
		if h.OnToken != nil {
			h.OnToken("done")
		}
		return &llm.StreamResult{Content: "done", Model: "glm-b"}, nil
	}
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	var events []string
	h := &llm.StreamHandlers{
		OnReplyModel: func(model string) { events = append(events, "reply_model:"+model) },
		OnStreamEnd:  func() { events = append(events, "stream_end") },
	}
	if _, err := a.StreamProcessInput(context.Background(), "read x", h); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}

	want := []string{"reply_model:glm-a", "stream_end", "reply_model:glm-b", "stream_end"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}

	// The session messages carry each round's model, so replay parity holds.
	var models []string
	for _, m := range a.Messages {
		if m.Role == "assistant" {
			models = append(models, m.Model)
		}
	}
	if got := strings.Join(models, ","); got != "glm-a,glm-b" {
		t.Fatalf("assistant models = %v, want [glm-a glm-b]", models)
	}
}
