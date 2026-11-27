package contextmgr

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestFoldSkipsToolResultsWithoutTheirCall pins the guard against a folded
// span that starts after the assistant message carrying the call: a region
// split can cut a pair, and Fold is exported, so a result with no call in the
// span used to fold as tool "unknown" with an empty key. That produced
// "- unknown \"\"" lines in BOTH the handles and the operation ledger — entries
// the manifest can never explain, re-imported on every restore.
func TestFoldSkipsToolResultsWithoutTheirCall(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	st := NewSessionState()
	st.Fold([]llm.Message{
		// The call is NOT in this span.
		{Role: "tool", ToolCallID: "call_head", Content: "Started background job job-bg9.\nCommand: make"},
	})
	if len(st.operations) != 0 {
		t.Fatalf("orphan result folded %d operations, want 0: %+v", len(st.operations), st.operations)
	}
	if len(st.handles) != 0 {
		t.Fatalf("orphan result folded %d handles, want 0: %+v", len(st.handles), st.handles)
	}
	if block := m.renderState(st, maxStateTokens); block != "" {
		t.Fatalf("orphan result produced a manifest:\n%s", block)
	}
	// The id must stay UNFOLDED, so a later span carrying the call folds the
	// pair correctly instead of skipping it as already seen.
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_head", Name: "execute_command",
			Args: map[string]any{"command": "make", "background": true},
		}}},
		{Role: "tool", ToolCallID: "call_head", Content: "Started background job job-bg9.\nCommand: make"},
	})
	block := m.renderState(st, maxStateTokens)
	for _, want := range []string{`- job "job-bg9": execute_command "make"`, `- execute_command "make" (ok, 1x)`} {
		if !strings.Contains(block, want) {
			t.Fatalf("re-folded span missing %q:\n%s", want, block)
		}
	}
}

// TestImportLaterBlockWins pins the merge rule between the blocks a transcript
// can hold: a deep-dive session summary plus the progress summaries written
// after it. They fold oldest first, so without "later wins" the OLDEST block
// won the handle status and the operation outcome — a restore reported a job
// that had since finished as still live, and a command that had since
// succeeded as failed.
func TestImportLaterBlockWins(t *testing.T) {
	older := StateHeader + "\n" +
		"live:\n" +
		`- job "job-bg1": execute_command "make build"` + "\n" +
		"failed:\n" +
		`- execute_command "make test": Error: exit status 1` + "\n" +
		"files:\n" +
		`- "main.go" (read, 1x)` + "\n" +
		StateFooter
	newer := StateHeader + "\n" +
		"recoverable:\n" +
		`- job "job-bg1": execute_command "make build"` + "\n" +
		"operations:\n" +
		`- execute_command "make test" (ok, 3x)` + "\n" +
		"files:\n" +
		`- "main.go" (modified, 2x)` + "\n" +
		StateFooter

	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", Content: SummaryPrefix + "session recap\n\n" + older},
		{Role: "assistant", Content: ProgressSummaryPrefix + "progress recap\n\n" + newer},
	})

	h := st.handles[stateKindJob+"\x00job-bg1"]
	if h == nil {
		t.Fatal("handle lost across the two blocks")
	}
	if h.Status != stateStatusRecoverable {
		t.Fatalf("handle status = %q, want recoverable (the newer block's snapshot)", h.Status)
	}
	op := st.operations["execute_command\x00make test"]
	if op == nil {
		t.Fatal("operation lost across the two blocks")
	}
	if op.Outcome != stateOutcomeOK {
		t.Fatalf("operation outcome = %q, want ok: the newer block recorded the retry that succeeded", op.Outcome)
	}
	if op.Evidence != "" {
		t.Fatalf("operation evidence = %q, want the newer block's (none)", op.Evidence)
	}
	if op.Count != 3 {
		t.Fatalf("operation count = %d, want 3", op.Count)
	}
	f := st.files["main.go"]
	if f == nil {
		t.Fatal("file lost across the two blocks")
	}
	if f.Change != "modified" || f.Count != 2 {
		t.Fatalf("file = (%q, %dx), want (modified, 2x)", f.Change, f.Count)
	}
}

// TestImportNeverOverridesLiveFold pins the other half of the merge rule: an
// entry folded in THIS process (Seq > 0) describes the live session and keeps
// its status, whatever an older block says. Without it a restore's stale
// "live" line could resurrect a job the current fold already saw finish.
func TestImportNeverOverridesLiveFold(t *testing.T) {
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "execute_command", Args: map[string]any{"command": "make build", "background": true},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "Started background job job-bg1.\nCommand: make build"},
		{Role: "user", Content: "[job] job-bg1 (make build) exited with code 0"},
	})
	stale := StateHeader + "\n" +
		"live:\n" +
		`- job "job-bg1": execute_command "make build"` + "\n" +
		StateFooter
	st.Fold([]llm.Message{{Role: "assistant", Content: SummaryPrefix + "old recap\n\n" + stale}})
	h := st.handles[stateKindJob+"\x00job-bg1"]
	if h == nil {
		t.Fatal("handle lost")
	}
	if h.Status != stateStatusDone {
		t.Fatalf("handle status = %q, want done: the live fold saw the job exit, the imported block is stale", h.Status)
	}
}
