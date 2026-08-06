package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestValidateImageInputs covers the acceptance rules for user-attached
// images: limits, data-URL sanity, and detail normalization.
func TestValidateImageInputs(t *testing.T) {
	valid := llm.ImageInput{DataURL: "data:image/png;base64,AAAA", Detail: "high"}
	if got, err := validateImageInputs(nil); err != nil || got != nil {
		t.Fatalf("nil input: got %v, err %v", got, err)
	}

	// Too many images.
	many := make([]llm.ImageInput, maxImagesPerMessage+1)
	for i := range many {
		many[i] = valid
	}
	if _, err := validateImageInputs(many); err == nil {
		t.Fatal("expected error for too many images")
	}

	cases := []struct {
		name string
		img  llm.ImageInput
	}{
		{"empty data url", llm.ImageInput{DataURL: ""}},
		{"non-image prefix", llm.ImageInput{DataURL: "data:text/plain;base64,AAAA"}},
		{"missing base64 marker", llm.ImageInput{DataURL: "data:image/png,AAAA"}},
		{"not a data url", llm.ImageInput{DataURL: "https://example.com/x.png"}},
		{"bad detail", llm.ImageInput{DataURL: "data:image/png;base64,AAAA", Detail: "ultra"}},
	}
	for _, tc := range cases {
		if _, err := validateImageInputs([]llm.ImageInput{tc.img}); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}

	// Detail normalization: empty becomes "" (defaulted upstream to auto),
	// mixed case is lowered.
	got, err := validateImageInputs([]llm.ImageInput{
		{DataURL: "data:image/png;base64,AAAA", Detail: "HIGH"},
		{DataURL: "data:image/jpeg;base64,BBBB"},
	})
	if err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if got[0].Detail != "high" {
		t.Fatalf("detail = %q, want normalized lower", got[0].Detail)
	}
	if got[1].Detail != "" {
		t.Fatalf("detail = %q, want empty", got[1].Detail)
	}
}

// TestStreamProcessInputWithImagesAppendsImageMessage verifies the agent
// stores the user message with its images so they survive persistence,
// rendering, and resend.
func TestStreamProcessInputWithImagesAppendsImageMessage(t *testing.T) {
	prov := llm.NewMockProvider()
	exec := agent.NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := agent.NewAgent(prov, exec, ctxMgr)

	images := []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA", Detail: "auto"}}
	if _, err := a.StreamProcessInputWithImages(context.Background(), "see this", images, nil); err != nil {
		t.Fatalf("StreamProcessInputWithImages: %v", err)
	}
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" && m.Content == "see this" {
			if len(m.Images) != 1 || m.Images[0].DataURL != "data:image/png;base64,AAAA" {
				t.Fatalf("user message images = %#v, want the attached image", m.Images)
			}
			return
		}
	}
	t.Fatal("user message with images not found in history")
}

// TestWSMessageWithImagesReachesAgent verifies the full web path: a message
// frame with images is validated, forwarded to the agent, and stored in the
// conversation so it persists and renders.
func TestWSMessageWithImagesReachesAgent(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	img := llm.ImageInput{DataURL: "data:image/png;base64,AAAA"}
	if err := conn.WriteJSON(WSMessage{
		Type:      "message",
		Content:   "what is this",
		Images:    []llm.ImageInput{img},
		SessionID: a.SessionID,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.releaseN(1)
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})

	found := false
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" && len(m.Images) == 1 && m.Images[0].DataURL == img.DataURL {
			found = true
		}
	}
	if !found {
		t.Fatal("user message with image not in agent history")
	}
}

// TestWSMessageRejectsInvalidImages verifies a malformed image frame is
// rejected with an error response and does not start a turn (no message is
// appended to the conversation).
func TestWSMessageRejectsInvalidImages(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{
		Type:      "message",
		Content:   "look",
		Images:    []llm.ImageInput{{DataURL: "https://example.com/x.png"}},
		SessionID: a.SessionID,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if !strings.Contains(resp.Content, "base64 data URL") {
		t.Fatalf("response = %q, want data-URL validation error", resp.Content)
	}
	if a.MessageCount() != 0 {
		t.Fatalf("rejected image frame still appended %d messages", a.MessageCount())
	}
}

// TestHistoryEntriesCarryImages verifies history snapshots include user
// images (including pure-image messages with empty text) so the web UI can
// render them after reload / pane focus.
func TestHistoryEntriesCarryImages(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "", Images: []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA"}}},
		{Role: "user", Content: "with text", Images: []llm.ImageInput{{DataURL: "data:image/jpeg;base64,BBBB"}}},
	}
	entries := historyEntries(msgs)
	if len(entries) != 2 {
		t.Fatalf("history entries = %d, want 2 (pure-image message must survive)", len(entries))
	}
	if len(entries[0].Images) != 1 || entries[0].Images[0].DataURL != "data:image/png;base64,AAAA" {
		t.Fatalf("entry[0] images = %#v", entries[0].Images)
	}
	if entries[0].Content != "" {
		t.Fatalf("pure-image entry content = %q, want empty", entries[0].Content)
	}
}
