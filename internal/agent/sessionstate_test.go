package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestSessionStateClassificationComesFromTheRegistry pins the injected
// file-mutation classification end to end, from the tool registry to the
// rendered manifest. It replaces the old cross-package test that compared a
// hardcoded copy of the list in contextmgr with agent.FSMutatingToolNames: the
// copy is gone (the agent passes its registry set into the state), so the
// property worth testing is behavioural — a MutatesFS tool's call renders its
// file as MODIFIED in the compaction summary, which is what tells a model
// resuming from the summary that it already rewrote the file.
//
// The table drives every builtin MutatesFS tool, so a write tool that is later
// added to the registry and flagged MutatesFS is covered without editing this
// test — and a state that stopped receiving the registry set would fail on all
// of them at once.
func TestSessionStateClassificationComesFromTheRegistry(t *testing.T) {
	// One representative argument set per MutatesFS builtin. The classification
	// is what is under test, so the file only has to be NAMED by the tool's
	// stateFileArg entry (see contextmgr.stateFileArg) for a files line to
	// appear; rename_symbol legitimately names no file and is asserted through
	// its operation line instead.
	// file is the path the manifest must render as modified, and "" for a tool
	// that names no file at all (rename_symbol takes old_name/new_name and a
	// search SCOPE, so it legitimately contributes no files entry).
	cases := map[string]struct {
		args map[string]any
		file string
	}{
		"write_file":      {map[string]any{"path": "gen/new.go", "content": "package gen"}, "gen/new.go"},
		"replace_in_file": {map[string]any{"path": "internal/agent/turn.go", "search": "a", "replace": "b"}, "internal/agent/turn.go"},
		"patch_file":      {map[string]any{"diff": "--- a/x\n+++ b/x\n", "path": "internal/agent/turn.go"}, "internal/agent/turn.go"},
		"delete":          {map[string]any{"path": "gone.go"}, "gone.go"},
		"download_file":   {map[string]any{"url": "https://example.com/x", "path": "vendor/x"}, "vendor/x"},
		"rename_symbol":   {map[string]any{"old_name": "a", "new_name": "b", "path": "internal/agent"}, ""},
	}

	for _, name := range FSMutatingToolNames() {
		name := name
		t.Run(name+" renders modified", func(t *testing.T) {
			tc, ok := cases[name]
			if !ok {
				t.Fatalf("%q is MutatesFS in the registry but the test has no argument set for it; "+
					"add one (and make sure contextmgr.stateFileArg names its file argument)", name)
			}
			toolArgs, path := tc.args, tc.file

			provider := &countingProvider{limit: 20000}
			ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
				ContextLimit:              20000,
				CompactKeepRecentMessages: 2,
			})
			a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

			a.appendMessage(llm.Message{Role: "user", Content: "please change it"})
			for i := 0; i < 2; i++ {
				a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
			}
			a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: "call_mut", Name: name, Args: toolArgs,
			}}})
			a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_mut", Content: "done"})
			a.appendMessage(llm.Message{Role: "assistant", Content: "changed"})
			a.appendMessage(llm.Message{Role: "user", Content: "thanks"})

			if err := a.CompactHistory(context.Background()); err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}
			summary := compactionSummary(t, a)
			if !strings.Contains(summary, contextmgr.StateHeader) {
				t.Fatalf("summary carries no state manifest:\n%s", summary)
			}
			// The operation line proves the call was folded at all; a tool that
			// names no file (rename_symbol) is only expected to appear there.
			if !strings.Contains(summary, "- "+name+" ") {
				t.Fatalf("summary does not record the %q call:\n%s", name, summary)
			}
			if path == "" {
				return
			}
			want := `- "` + path + `" (modified, 1x)`
			if !strings.Contains(summary, want) {
				t.Fatalf("summary does not render %q as modified; a model resuming from it "+
					"believes it only read the file it rewrote:\n%s", path, summary)
			}
			if strings.Contains(summary, `- "`+path+`" (read`) {
				t.Fatalf("summary classified the %q edit as a read:\n%s", name, summary)
			}
		})
	}
}

// TestSessionStateReadToolStaysRead pins the other direction: a tool the
// registry does not flag MutatesFS keeps rendering as a read, so the injected
// set is consulted rather than ignored (or replaced by "everything is
// modified").
func TestSessionStateReadToolStaysRead(t *testing.T) {
	provider := &countingProvider{limit: 20000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              20000,
		CompactKeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	a.appendMessage(llm.Message{Role: "user", Content: "have a look"})
	for i := 0; i < 2; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "call_read", Name: "read_file", Args: map[string]any{"path": "internal/agent/turn.go"},
	}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_read", Content: "package agent"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "read it"})
	a.appendMessage(llm.Message{Role: "user", Content: "thanks"})

	if err := a.CompactHistory(context.Background()); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	summary := compactionSummary(t, a)
	if want := `- "internal/agent/turn.go" (read, 1x)`; !strings.Contains(summary, want) {
		t.Fatalf("summary does not render the read as %q:\n%s", want, summary)
	}
}

// compactionSummary returns the compaction summary in the agent's history.
func compactionSummary(t *testing.T, a *Agent) string {
	t.Helper()
	for _, msg := range a.SnapshotMessages() {
		if contextmgr.IsCompactionSummary(msg.Content) {
			return msg.Content
		}
	}
	t.Fatal("no compaction summary in history")
	return ""
}
