package llm

import (
	"encoding/json"
	"testing"
)

func TestMessagesToChatRoundTripsReasoningAndRefusal(t *testing.T) {
	t.Parallel()
	p := &OpenAIProvider{}
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{
			Role:      "assistant",
			Content:   "hello",
			Reasoning: "thinking step",
			Refusal:   "",
		},
		{
			Role:    "assistant",
			Refusal: "I cannot help with that.",
		},
		{
			Role:      "assistant",
			Reasoning: "plan the tool call",
			ToolCalls: []ToolCall{{
				ID:      "c1",
				Name:    "read_file",
				ArgsStr: `{"path":"a.go"}`,
			}},
		},
	}
	chat := p.messagesToChat(msgs)
	if len(chat) != 4 {
		t.Fatalf("len(chat) = %d", len(chat))
	}

	asstWithBoth, err := json.Marshal(chat[1])
	if err != nil {
		t.Fatal(err)
	}
	var body1 map[string]any
	if err := json.Unmarshal(asstWithBoth, &body1); err != nil {
		t.Fatal(err)
	}
	if body1["content"] != "hello" {
		t.Fatalf("content = %#v", body1["content"])
	}
	if body1["reasoning_content"] != "thinking step" {
		t.Fatalf("reasoning_content = %#v", body1["reasoning_content"])
	}
	if _, ok := body1["refusal"]; ok {
		t.Fatalf("refusal should be omitted when empty, got %#v", body1["refusal"])
	}

	asstRefusal, err := json.Marshal(chat[2])
	if err != nil {
		t.Fatal(err)
	}
	var body2 map[string]any
	if err := json.Unmarshal(asstRefusal, &body2); err != nil {
		t.Fatal(err)
	}
	if body2["refusal"] != "I cannot help with that." {
		t.Fatalf("refusal = %#v", body2["refusal"])
	}
	if _, ok := body2["content"]; ok {
		t.Fatalf("content should be omitted when empty, got %#v", body2["content"])
	}

	asstTools, err := json.Marshal(chat[3])
	if err != nil {
		t.Fatal(err)
	}
	var body3 map[string]any
	if err := json.Unmarshal(asstTools, &body3); err != nil {
		t.Fatal(err)
	}
	if body3["reasoning_content"] != "plan the tool call" {
		t.Fatalf("tool-call reasoning_content = %#v", body3["reasoning_content"])
	}
	if _, ok := body3["tool_calls"]; !ok {
		t.Fatalf("expected tool_calls, got %#v", body3)
	}
}
