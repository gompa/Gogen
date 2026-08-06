package llm

import (
	"encoding/json"
	"testing"
)

// TestMessagesToChatUserImages verifies that a user message with images
// serializes to the OpenAI multi-part content form: a text part plus one
// image_url part per image, with the detail level forwarded.
func TestMessagesToChatUserImages(t *testing.T) {
	p := &OpenAIProvider{}
	msgs := []Message{
		{
			Role:    "user",
			Content: "what is this?",
			Images: []ImageInput{
				{DataURL: "data:image/png;base64,AAAA", Detail: "high"},
				{DataURL: "data:image/jpeg;base64,BBBB"},
			},
		},
	}
	chat := p.messagesToChat(msgs)
	if len(chat) != 1 {
		t.Fatalf("len(chat) = %d", len(chat))
	}
	raw, err := json.Marshal(chat[0])
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["role"] != "user" {
		t.Fatalf("role = %#v", body["role"])
	}
	parts, ok := body["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v (want array of parts)", body["content"])
	}
	if len(parts) != 3 {
		t.Fatalf("content parts = %d, want 3 (text + 2 images)", len(parts))
	}
	text, _ := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is this?" {
		t.Fatalf("text part = %#v", text)
	}
	img1, _ := parts[1].(map[string]any)
	if img1["type"] != "image_url" {
		t.Fatalf("image part type = %#v", img1["type"])
	}
	u1, _ := img1["image_url"].(map[string]any)
	if u1["url"] != "data:image/png;base64,AAAA" || u1["detail"] != "high" {
		t.Fatalf("image_url part = %#v", u1)
	}
	img2, _ := parts[2].(map[string]any)
	u2, _ := img2["image_url"].(map[string]any)
	if u2["url"] != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("second image_url part = %#v", u2)
	}
	if d, ok := u2["detail"]; ok && d != "auto" {
		t.Fatalf("empty detail should default to auto, got %#v", d)
	}
}

// TestMessagesToChatPureImageUser verifies that a user message with images
// but no text omits the text part entirely.
func TestMessagesToChatPureImageUser(t *testing.T) {
	p := &OpenAIProvider{}
	msgs := []Message{
		{Role: "user", Images: []ImageInput{{DataURL: "data:image/png;base64,AAAA"}}},
	}
	raw, err := json.Marshal(p.messagesToChat(msgs)[0])
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	parts := body["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("content parts = %d, want 1 image part", len(parts))
	}
	img, _ := parts[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("part = %#v", img)
	}
}

// TestMessagesToChatTextOnlyUntouched verifies the image path never changes
// the wire form of text-only user messages.
func TestMessagesToChatTextOnlyUntouched(t *testing.T) {
	p := &OpenAIProvider{}
	msgs := []Message{{Role: "user", Content: "plain text"}}
	raw, err := json.Marshal(p.messagesToChat(msgs)[0])
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["content"] != "plain text" {
		t.Fatalf("text-only user message changed shape: %#v", body["content"])
	}
}
