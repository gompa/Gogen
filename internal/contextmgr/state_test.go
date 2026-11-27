package contextmgr

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// stateTestMessages is a mixed span: a background job start, a file read, and
// a failed patch of the same file.
func stateTestMessages() []llm.Message {
	return []llm.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "execute_command", Args: map[string]any{"command": "make build", "background": true}},
			{ID: "c2", Name: "read_file", Args: map[string]any{"path": "src/foo.go"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "Started background job job-bg1.\nCommand: make build"},
		{Role: "tool", ToolCallID: "c2", Content: "package foo"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c3", Name: "patch_file", Args: map[string]any{"path": "src/foo.go"}},
		}},
		{Role: "tool", ToolCallID: "c3", Content: "Error: hunk failed"},
	}
}

func TestStateFoldExtractsHandlesOperationsAndFiles(t *testing.T) {
	st := NewSessionState()
	st.Fold(stateTestMessages())
	if st.Empty() {
		t.Fatal("state is empty after folding executed calls")
	}

	job := st.handles[stateKindJob+"\x00job-bg1"]
	if job == nil || job.Status != stateStatusLive {
		t.Fatalf("job handle = %+v, want live", job)
	}
	if job.Label != `execute_command "make build"` {
		t.Fatalf("job label = %q", job.Label)
	}

	op := st.operations["execute_command\x00make build"]
	if op == nil || op.Count != 1 || op.Outcome != stateOutcomeOK {
		t.Fatalf("execute operation = %+v", op)
	}
	failed := st.operations["patch_file\x00src/foo.go"]
	if failed == nil || failed.Outcome != stateOutcomeFailed || !strings.Contains(failed.Evidence, "hunk failed") {
		t.Fatalf("patch operation = %+v, want failed with evidence", failed)
	}

	file := st.files["src/foo.go"]
	if file == nil || file.Change != "modified" || file.Count != 2 {
		t.Fatalf("file = %+v, want modified x2", file)
	}
}

func TestStateFoldIsIdempotent(t *testing.T) {
	st := NewSessionState()
	msgs := stateTestMessages()
	st.Fold(msgs)
	st.Fold(msgs)
	if got := st.operations["read_file\x00src/foo.go"].Count; got != 1 {
		t.Fatalf("read count after two folds = %d, want 1", got)
	}
	if got := st.files["src/foo.go"].Count; got != 2 {
		t.Fatalf("file count after two folds = %d, want 2", got)
	}
}

func TestStateFoldNoticeClosesJobHandle(t *testing.T) {
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "execute_command", Args: map[string]any{"command": "make build"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "Started background job job-bg1.\nCommand: make build"},
		{Role: "user", Content: JobNoticePrefix + "job-bg1 (make build) exited with code 0"},
	})
	job := st.handles[stateKindJob+"\x00job-bg1"]
	if job == nil || job.Status != stateStatusDone {
		t.Fatalf("job handle = %+v, want done after the notice", job)
	}
	if st.Empty() {
		t.Fatal("the operation ledger keeps the state non-empty")
	}
}

// TestStateFoldKeepsTJobIDIntact pins the regex alternation order: a
// terminal background send id (tjob-...) must not be captured as a plain
// job-... substring, or the manifest would advertise an unusable handle.
func TestStateFoldKeepsTJobIDIntact(t *testing.T) {
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "terminal", Args: map[string]any{"action": "send", "session_id": "term-1a2b"},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "Started background terminal send tjob-9z8y on terminal term-1a2b."},
	})
	if _, ok := st.handles[stateKindJob+"\x00tjob-9z8y"]; !ok {
		t.Fatalf("tjob id not captured intact: %v", st.handles)
	}
	if _, ok := st.handles[stateKindJob+"\x00job-9z8y"]; ok {
		t.Fatal("tjob id was captured as a truncated job id")
	}
}

// TestStateFoldFailedJobNoticeIsNegativeConstraint pins the "do not retry"
// record for a background command that exits non-zero: the notice lands in
// the failed section, not only on the (unrendered) job handle.
func TestStateFoldFailedJobNoticeIsNegativeConstraint(t *testing.T) {
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "execute_command", Args: map[string]any{"command": "make test", "background": true},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "Started background job job-bg9.\nCommand: make test"},
		{Role: "user", Content: JobNoticePrefix + "job-bg9 (make test) exited with code 2"},
	})
	op := st.operations["background_job\x00job-bg9"]
	if op == nil || op.Outcome != stateOutcomeFailed {
		t.Fatalf("failed job notice not in the operation ledger: %+v", op)
	}
	block := NewManager(nil, DefaultSettings()).renderState(st, maxStateTokens)
	if !strings.Contains(block, "failed:") || !strings.Contains(block, "job-bg9") {
		t.Fatalf("failed job missing from the manifest:\n%s", block)
	}
}

func TestStateImportRoundTrip(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	st := NewSessionState()
	st.Fold(stateTestMessages())
	block := m.renderState(st, maxStateTokens)
	if block == "" {
		t.Fatal("renderState returned an empty manifest")
	}

	restored := NewSessionState()
	restored.importBlock(block)
	job := restored.handles[stateKindJob+"\x00job-bg1"]
	if job == nil || job.Status != stateStatusLive {
		t.Fatalf("imported job = %+v, want live", job)
	}
	if op := restored.operations["execute_command\x00make build"]; op == nil || op.Count != 1 {
		t.Fatalf("imported operation = %+v", op)
	}
	if file := restored.files["src/foo.go"]; file == nil || file.Count != 2 {
		t.Fatalf("imported file = %+v", file)
	}
}

// TestStateFoldImportsPreviousSummary pins the restore/fork path: a summary
// that already carries a state block seeds the cumulative memory instead of
// starting empty.
func TestStateFoldImportsPreviousSummary(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	first := NewSessionState()
	first.Fold(stateTestMessages())
	block := m.renderState(first, maxStateTokens)

	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: SummaryPrefix + "old recap\n\n" + block},
	})
	if job := st.handles[stateKindJob+"\x00job-bg1"]; job == nil || job.Status != stateStatusLive {
		t.Fatalf("imported job = %+v, want live", job)
	}
}

func TestStateRenderPrioritizesActionableEntries(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	st := NewSessionState()
	st.ensure()
	st.seq = 100
	st.upsertHandle(stateKindJob, "job-bg1", stateStatusLive, `execute_command "make build"`)
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("operation-%02d-%s", i, strings.Repeat("x", 40))
		st.operations["read_file\x00"+key] = &StateOperation{
			Tool: "read_file", Key: key, Outcome: stateOutcomeOK, Count: 1, Seq: i,
		}
	}
	block := m.renderState(st, 60)
	if !strings.Contains(block, `- job "job-bg1"`) {
		t.Fatalf("live handle missing from a small manifest:\n%s", block)
	}
	if !strings.Contains(block, "more state entries omitted") {
		t.Fatalf("small manifest did not report omissions:\n%s", block)
	}
	if strings.Contains(block, "operation-29") {
		t.Fatalf("low-priority operation took budget from the live handle:\n%s", block)
	}
}

func TestStripStateBlock(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	st := NewSessionState()
	st.Fold(stateTestMessages())
	block := m.renderState(st, maxStateTokens)

	content := "recap\n\n" + block + "\n\n[Paused before execution]"
	stripped := StripStateBlock(content)
	if strings.Contains(stripped, StateHeader) || strings.Contains(stripped, StateFooter) {
		t.Fatalf("state block survived stripping:\n%s", stripped)
	}
	if stripped != "recap\n\n[Paused before execution]" {
		t.Fatalf("stripped = %q", stripped)
	}
	if got := StripStateBlock("plain recap"); got != "plain recap" {
		t.Fatalf("plain content changed: %q", got)
	}
}

// TestCompactSummaryStripsPreviousStateFromRequest pins the input hygiene:
// the summarization request must not feed the previous machine block back to
// the model, while the new summary carries a fresh block.
func TestCompactSummaryStripsPreviousStateFromRequest(t *testing.T) {
	provider := &recordingProvider{}
	m := NewManager(provider, Settings{CompactKeepRecentMessages: 2})
	m.minMiddleTokens = 0
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: SummaryPrefix + "old recap\n\n" +
			StateHeader + "\nlive:\n- job \"job-old\": x\n" + StateFooter},
		{Role: "assistant", Content: strings.Repeat("filler ", 240)},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": "main.go"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "package main"},
		{Role: "assistant", Content: "tail one"},
		{Role: "user", Content: "tail two"},
	}
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("no summarization request recorded")
	}
	for _, req := range provider.requests {
		for _, msg := range req {
			if strings.Contains(msg.Content, StateHeader) {
				t.Fatalf("summarization request carries the old state block:\n%s", msg.Content)
			}
		}
	}
	summary := out[1].Content
	if !strings.Contains(summary, StateHeader) || !strings.Contains(summary, `"job-old"`) {
		t.Fatalf("new summary missing the cumulative state block:\n%s", summary)
	}
}

// TestDeepDiveStateManifestOnlyInProgressSummary pins the cache placement:
// the session summary is kept byte-identical across compactions, so the
// manifest goes into the progress summary only.
func TestDeepDiveStateManifestOnlyInProgressSummary(t *testing.T) {
	p := &regionProvider{}
	m := newDeepDiveManager(p, true)
	current := llm.Message{Role: "user", Content: "current request"}
	msgs := []llm.Message{{Role: "user", Content: "first"}, bulky("old"), current}
	msgs = append(msgs,
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "execute_command", Args: map[string]any{"command": "make build", "background": true}},
		}},
		llm.Message{Role: "tool", ToolCallID: "c1", Content: "Started background job job-bg1.\nCommand: make build"},
	)
	msgs = append(msgs, rounds("r0", 3)...)

	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var sessionSummary, progressSummary string
	for _, msg := range out {
		switch {
		case strings.HasPrefix(msg.Content, SummaryPrefix):
			sessionSummary = msg.Content
		case strings.HasPrefix(msg.Content, ProgressSummaryPrefix):
			progressSummary = msg.Content
		}
	}
	if sessionSummary == "" || progressSummary == "" {
		t.Fatalf("expected both summaries, got session=%q progress=%q", sessionSummary, progressSummary)
	}
	if strings.Contains(sessionSummary, StateHeader) {
		t.Fatalf("session summary carries a state block (breaks the byte-identical prefix):\n%s", sessionSummary)
	}
	if !strings.Contains(progressSummary, StateHeader) || !strings.Contains(progressSummary, "job-bg1") {
		t.Fatalf("progress summary missing the state manifest:\n%s", progressSummary)
	}
}
