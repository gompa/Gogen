//go:build debug

package agent

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestMessagesEqualDetectsReasoningDrift(t *testing.T) {
	a := &llm.Message{Role: "assistant", Content: "hi", Reasoning: "think A"}
	b := &llm.Message{Role: "assistant", Content: "hi", Reasoning: "think B"}
	if messagesEqual(a, b) {
		t.Fatal("Reasoning should affect wire-view equality (sent as reasoning_content)")
	}
}

func TestMessagesEqualDetectsRefusalDrift(t *testing.T) {
	a := &llm.Message{Role: "assistant", Refusal: "nope"}
	b := &llm.Message{Role: "assistant", Refusal: "different"}
	if messagesEqual(a, b) {
		t.Fatal("Refusal should affect wire-view equality")
	}
}

func TestMessagesEqualDetectsArgsStrDrift(t *testing.T) {
	a := &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", ArgsStr: `{"path":"a"}`}}}
	b := &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", ArgsStr: `{"path":"b"}`}}}
	if messagesEqual(a, b) {
		t.Fatal("expected ArgsStr mismatch")
	}
}

func TestCloneViewMessagesDetachesToolCalls(t *testing.T) {
	orig := []llm.Message{{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:      "1",
			Name:    "read_file",
			ArgsStr: `{"path":"a"}`,
		}},
	}}
	cloned := cloneViewMessages(orig)
	orig[0].ToolCalls[0].ArgsStr = `{"path":"mutated"}`
	if cloned[0].ToolCalls[0].ArgsStr != `{"path":"a"}` {
		t.Fatalf("clone shares ToolCalls backing: %q", cloned[0].ToolCalls[0].ArgsStr)
	}
}

func TestCompareViewFingerprintsIgnoresAppendOnlyGrowth(t *testing.T) {
	a := &Agent{
		DebugCompareMessages: true,
		lastViewMessages: cloneViewMessages([]llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		}),
	}
	// Longer view with identical prefix must not panic and must not treat
	// append-only growth as cache-busting drift.
	current := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok"},
	}
	a.compareViewFingerprints(current) // must not panic
}

func TestMessageWireHashIsStableAcrossReallocs(t *testing.T) {
	m := llm.Message{Role: "system", Content: "sys"}
	h1 := messageWireHash(&m)
	cp := m
	h2 := messageWireHash(&cp)
	if h1 == "" {
		t.Fatal("empty hash")
	}
	if h1 != h2 {
		t.Fatalf("hash not content-keyed: %q != %q", h1, h2)
	}
	m.Content = "other"
	if messageWireHash(&m) == h1 {
		t.Fatal("hash should change when content changes")
	}
}

func TestCompareViewFingerprintsSystemMessageDriftDetected(t *testing.T) {
	a := &Agent{
		DebugCompareMessages: true,
		lastViewMessages: cloneViewMessages([]llm.Message{
			{Role: "system", Content: "sys A"},
			{Role: "user", Content: "hi"},
		}),
	}
	current := []llm.Message{
		{Role: "system", Content: "sys B"}, // index 0 mutation → bust whole prefix
		{Role: "user", Content: "hi"},
	}
	// Must not panic; compareViewFingerprints now also emits system/message hashes.
	a.compareViewFingerprints(current)
}

func TestRestoreSessionLocalComparesPreviousView(t *testing.T) {
	a := &Agent{
		DebugCompareMessages: true,
		SessionID:            "sess-old",
		WorkingDir:           "/tmp/project",
		Mode:                 ModeAct,
		projectProfile:       "profile-sticky",
		lastViewMessages: cloneViewMessages([]llm.Message{
			{Role: "system", Content: "sys from previous session"},
			{Role: "user", Content: "old question"},
			{Role: "assistant", Content: "old answer"},
		}),
		Messages: []llm.Message{
			{Role: "user", Content: "old question"},
			{Role: "assistant", Content: "old answer"},
		},
	}

	a.RestoreSessionLocal(SessionSnapshot{
		WorkingDir:     "/tmp/project",
		ProjectProfile: "profile-sticky",
		Mode:           "act",
		Messages: []llm.Message{
			{Role: "user", Content: "new session question"},
			{Role: "assistant", Content: "new session answer"},
		},
	}, "sess-new")

	if len(a.lastViewMessages) == 0 {
		t.Fatal("expected restored wire view to be snapshotted for later turn drift")
	}
	// Restored snapshot should reflect the NEW session, not the old one.
	foundNew := false
	for _, m := range a.lastViewMessages {
		if m.Role == "user" && m.Content == "new session question" {
			foundNew = true
		}
		if m.Role == "user" && m.Content == "old question" {
			t.Fatal("lastViewMessages still holds previous session user message")
		}
	}
	if !foundNew {
		t.Fatalf("restored snapshot missing new session content: %#v", a.lastViewMessages)
	}
}

func TestRestoreSessionLocalSnapshotsWithoutPreviousView(t *testing.T) {
	a := &Agent{
		DebugCompareMessages: true,
		WorkingDir:           "/tmp/project",
		Mode:                 ModeAct,
	}
	a.RestoreSessionLocal(SessionSnapshot{
		WorkingDir: "/tmp/project",
		Mode:       "act",
		Messages:   []llm.Message{{Role: "user", Content: "only"}},
	}, "sess-a")
	if len(a.lastViewMessages) == 0 {
		t.Fatal("expected snapshot even with no previous view")
	}
}

func TestReportViewDriftSessionRestoreAlwaysLogs(t *testing.T) {
	a := &Agent{
		DebugCompareMessages: true,
		lastViewMessages: cloneViewMessages([]llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "shared"},
		}),
	}
	// Identical overlapping prefix — turn drift would stay silent; session
	// restore must still accept the call (log is gated by GOGEN_DEBUG_LOG).
	current := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "shared"},
	}
	a.reportViewDrift(current, viewDriftSessionRestore, "old", "new")
}

func TestDriftPreviewTruncatesAndEscapes(t *testing.T) {
	got := driftPreview("line1\nline2\t" + strings.Repeat("x", 300))
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Fatalf("raw control chars leaked: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncation marker: %q", got)
	}
}

func TestMessageChangedFieldsDetectsToolArgs(t *testing.T) {
	prev := &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", ArgsStr: `{"path":"a"}`}}}
	cur := &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", ArgsStr: `{"path":"b"}`}}}
	got := messageChangedFields(prev, cur)
	if len(got) != 1 || got[0] != "toolCalls" {
		t.Fatalf("changedFields = %v", got)
	}
}

func TestViewDriftCompiledInDebug(t *testing.T) {
	if !ViewDriftCompiledIn() {
		t.Fatal("expected ViewDriftCompiledIn in debug builds")
	}
}
