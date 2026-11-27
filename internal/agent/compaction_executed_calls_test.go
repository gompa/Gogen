package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestCompactHistoryKeepsSessionStateHandle pins the agent-level end-to-end
// behavior behind the compaction manifest: when a background execute call is
// summarized away, the JOB id from its result survives in the summary (the
// wire call id does not: no tool takes it after the pair is gone). Without
// the manifest the model would see a condensed span and silently lose the
// handle it needs to address the running job (background_job wait/list,
// terminal send) on a later turn.
func TestCompactHistoryKeepsSessionStateHandle(t *testing.T) {
	provider := &countingProvider{limit: 20000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              20000,
		CompactKeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	a.appendMessage(llm.Message{Role: "user", Content: "start a long build"})
	// Two large middle messages so the summarized span clears the
	// minimum-middle guard (defaultMinMiddleTokens).
	for i := 0; i < 2; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID:   "call_bg1",
		Name: "execute_command",
		Args: map[string]any{"command": "make build", "background": true},
	}}})
	a.appendMessage(llm.Message{
		Role:       "tool",
		ToolCallID: "call_bg1",
		Content:    "Started background job job-bg1.\nCommand: make build",
	})
	a.appendMessage(llm.Message{Role: "assistant", Content: "build started in the background"})
	a.appendMessage(llm.Message{Role: "user", Content: "check it later"})

	if err := a.CompactHistory(context.Background()); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	var summary string
	for _, msg := range a.SnapshotMessages() {
		if contextmgr.IsCompactionSummary(msg.Content) {
			summary = msg.Content
			break
		}
	}
	if summary == "" {
		t.Fatal("no compaction summary in history")
	}
	for _, want := range []string{
		contextmgr.StateHeader,
		`- job "job-bg1": execute_command "make build"`,
		`- execute_command "make build" (ok, 1x)`,
		contextmgr.StateFooter,
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	// The wire call id has no consumer after compaction; the job id does.
	if strings.Contains(summary, "call_bg1") {
		t.Fatalf("manifest carries the wire call id:\n%s", summary)
	}
}
