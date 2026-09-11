package agent

// Spill-storage integration tests: oversized tool output is persisted to
// the session's spill dir and the inline result becomes a head/tail
// preview + locator instead of a plain lossy truncation. Every failure
// path must fall back to the exact pre-spill behavior (the plain
// truncation marker), so these tests pin both the new contract and the
// fallbacks.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/spill"
)

// newSpillTestAgent returns an agent over a temp working dir with the
// given session id and context/tool-result cap (bytes). The executor's
// command-output cap is set to the same value (mirroring setup.go, which
// binds both to max_tool_result_bytes).
func newSpillTestAgent(t *testing.T, sessionID string, cap int) *Agent {
	t.Helper()
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetMaxToolOutputBytes(cap)
	settings := contextmgr.Settings{MaxToolResultBytes: cap}
	mgr := contextmgr.NewManager(llmtest.NewMockProvider(), settings)
	a := NewAgent(llmtest.NewMockProvider(), exec, mgr)
	a.SessionID = sessionID
	return a
}

// extractSpillPath pulls the persisted path out of a preview's locator.
func extractSpillPath(t *testing.T, result string) string {
	t.Helper()
	const marker = "full output saved to "
	i := strings.Index(result, marker)
	if i < 0 {
		t.Fatalf("no spill locator in result: %q", headStr(result, 200))
	}
	rest := result[i+len(marker):]
	end := strings.Index(rest, " — use read_file")
	if end < 0 {
		t.Fatalf("locator has no retrieval hint: %q", headStr(rest, 200))
	}
	return rest[:end]
}

func headStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestExecuteCommandSpillsOversizedOutput runs the full wiring (tool
// handler → executor → writer → cap): a command producing far more than
// the cap must keep its head AND tail in the inline result, carry the
// locator + retrieval hint, stay within the cap, and persist the complete
// output to the session's spill dir.
func TestExecuteCommandSpillsOversizedOutput(t *testing.T) {
	a := newSpillTestAgent(t, "spill-cmd-test", 512)
	// 200 lines × 17 bytes = 3400 bytes of output.
	cmd := "i=0; while [ $i -lt 200 ]; do echo 0123456789ABCDEF; i=$((i+1)); done"
	out, err := a.executeTool(context.Background(), llm.ToolCall{ID: "c1", Name: "execute_command", Args: map[string]any{"command": cmd}})
	if err != nil {
		t.Fatalf("executeTool: %v", err)
	}
	if len(out) > 512 {
		t.Fatalf("result is %d bytes, exceeds cap 512", len(out))
	}
	if !strings.Contains(out, "full output saved to") {
		t.Fatalf("result has no spill locator: %q", headStr(out, 200))
	}
	if !strings.Contains(out, "0123456789ABCDEF") {
		t.Fatalf("result lost the head: %q", headStr(out, 200))
	}
	if !contextmgr.HasTruncationMarker(out) {
		t.Fatal("result lost the standard truncation marker prefix")
	}
	// The full output must be on disk at the locator's path.
	path := extractSpillPath(t, out)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file unreadable: %v", err)
	}
	if len(data) != 3400 {
		t.Fatalf("spilled output = %d bytes, want the full 3400", len(data))
	}
	if !strings.HasSuffix(string(data), "0123456789ABCDEF\n") {
		t.Fatal("spilled output lost its tail")
	}
	// The tail of the spilled output must appear in the inline preview
	// (the last line survives even though the middle was cut).
	if !strings.Contains(out, "0123456789ABCDEF") {
		t.Fatal("preview lost the tail view")
	}
	// The spill landed in THIS session's dir.
	if want := spill.NewStore(a.WorkingDir).Dir("spill-cmd-test"); filepath.Dir(path) != want {
		t.Fatalf("spill dir = %q, want %q", filepath.Dir(path), want)
	}
}

// TestExecuteCommandSpillFailureFallsBackToMarker pins the best-effort
// contract: when the spill save fails (here: the spill root is a regular
// file, so the session dir cannot be created), the result must be the
// plain truncation marker — exactly the pre-spill behavior — and must not
// carry a locator to a file that does not exist.
func TestExecuteCommandSpillFailureFallsBackToMarker(t *testing.T) {
	a := newSpillTestAgent(t, "spill-fail-test", 256)
	// Block spilling: make .gogen/spill a FILE.
	root := filepath.Join(a.WorkingDir, ".gogen", "spill")
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := a.executeTool(context.Background(), llm.ToolCall{ID: "c1", Name: "execute_command", Args: map[string]any{"command": "i=0; while [ $i -lt 100 ]; do echo 0123456789ABCDEF; i=$((i+1)); done"}})
	if err != nil {
		t.Fatalf("executeTool: %v", err)
	}
	if !strings.Contains(out, "command output exceeds 256 bytes") {
		t.Fatalf("fallback must use the plain command-output marker: %q", headStr(out, 300))
	}
	if strings.Contains(out, "saved to") {
		t.Fatalf("failed spill must not advertise a locator: %q", headStr(out, 300))
	}
}

// TestAppendToolResultSpillsOversizedResult covers the generic tool seam:
// any tool result over the context cap is spilled whole and stored as a
// preview that references the spill file.
func TestAppendToolResultSpillsOversizedResult(t *testing.T) {
	a := newSpillTestAgent(t, "spill-result-test", 300)
	big := strings.Repeat("result-data\n", 100) // 1200 bytes
	a.appendToolResult(llm.ToolCall{ID: "call_1", Name: "read_files"}, big)
	if len(a.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(a.Messages))
	}
	msg := a.Messages[0]
	if msg.Role != "tool" || msg.ToolCallID != "call_1" {
		t.Fatalf("unexpected message: role=%q toolCallID=%q", msg.Role, msg.ToolCallID)
	}
	if len(msg.Content) > 300 {
		t.Fatalf("stored result is %d bytes, exceeds cap 300", len(msg.Content))
	}
	if !strings.Contains(msg.Content, "full output saved to") {
		t.Fatalf("stored result has no locator: %q", headStr(msg.Content, 200))
	}
	path := extractSpillPath(t, msg.Content)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file unreadable: %v", err)
	}
	if string(data) != big {
		t.Fatalf("spilled content = %d bytes, want the full %d", len(data), len(big))
	}
}

// TestAppendToolResultSkipsAlreadyTruncated pins the upstream-marker rule:
// a result already carrying the truncation marker was truncated upstream
// (its tail is gone — for commands the executor already spilled it), so
// the seam must not spill a partial copy or re-truncate it.
func TestAppendToolResultSkipsAlreadyTruncated(t *testing.T) {
	a := newSpillTestAgent(t, "spill-skip-test", 300)
	marked := strings.Repeat("x", 1000) + "\n… truncated (1000 bytes total)"
	a.appendToolResult(llm.ToolCall{ID: "call_1", Name: "execute_command"}, marked)
	if got := a.Messages[0].Content; got != marked {
		t.Fatalf("already-truncated result was modified: %d vs %d bytes", len(got), len(marked))
	}
	// No spill file was created for the partial content.
	dir := spill.NewStore(a.WorkingDir).Dir("spill-skip-test")
	if entries, err := os.ReadDir(dir); !os.IsNotExist(err) && len(entries) != 0 {
		t.Fatalf("spill files created for an upstream-truncated result: %v", entries)
	}
}

// TestAppendToolResultWithoutSessionKeepsLegacyCap pins the no-session
// fallback: bare agents (no session id — most unit tests and oneshot
// runs) keep the exact pre-spill cap behavior and write nothing to disk.
func TestAppendToolResultWithoutSessionKeepsLegacyCap(t *testing.T) {
	a := newSpillTestAgent(t, "", 300)
	big := strings.Repeat("y", 1000)
	a.appendToolResult(llm.ToolCall{ID: "call_1", Name: "read_file"}, big)
	got := a.Messages[0].Content
	if len(got) > 300 {
		t.Fatalf("stored result is %d bytes, exceeds cap 300", len(got))
	}
	if !contextmgr.HasTruncationMarker(got) || strings.Contains(got, "saved to") {
		t.Fatalf("legacy cap expected (plain marker, no locator): %q", headStr(got, 200))
	}
	// Legacy marker budget: marker reserved inside the cap.
	if want := "\n… truncated (1000 bytes total)"; !strings.HasSuffix(strings.TrimSpace(got), want) && !strings.Contains(got, want) {
		t.Fatalf("legacy marker missing: %q", headStr(got, 200))
	}
	if _, err := os.Stat(filepath.Join(a.WorkingDir, ".gogen", "spill")); !os.IsNotExist(err) {
		t.Fatal("no-session agent must not create a spill dir")
	}
}

// TestAppendToolResultSmallResultUntouched: under the cap nothing changes
// (the most common case — the seam must be invisible for small results).
func TestAppendToolResultSmallResultUntouched(t *testing.T) {
	a := newSpillTestAgent(t, "spill-small-test", 300)
	small := "small result"
	a.appendToolResult(llm.ToolCall{ID: "call_1", Name: "read_file"}, small)
	if got := a.Messages[0].Content; got != small {
		t.Fatalf("small result was modified: %q", got)
	}
	if _, err := os.Stat(filepath.Join(a.WorkingDir, ".gogen", "spill")); !os.IsNotExist(err) {
		t.Fatal("spill dir created for a small result")
	}
}

// TestSpillGateOffFallsBackToLegacyCap pins the config gate
// (output_spill): with spilling disabled, oversized results take the exact
// legacy plain-cap path — plain marker, no locator, no spill files — and
// re-enabling restores the spill behavior (the gate is read per call, so a
// live config change needs no agent rebuild).
func TestSpillGateOffFallsBackToLegacyCap(t *testing.T) {
	prev := OutputSpillEnabled()
	ConfigureOutputSpill(false)
	t.Cleanup(func() { ConfigureOutputSpill(prev) })

	a := newSpillTestAgent(t, "spill-gate-off", 300)
	big := strings.Repeat("z", 1000)
	a.appendToolResult(llm.ToolCall{ID: "call_1", Name: "read_file"}, big)
	got := a.Messages[0].Content
	if len(got) > 300 {
		t.Fatalf("stored result is %d bytes, exceeds cap 300", len(got))
	}
	if !contextmgr.HasTruncationMarker(got) || strings.Contains(got, "saved to") {
		t.Fatalf("gate off must use the legacy plain cap (no locator): %q", headStr(got, 200))
	}
	if _, err := os.Stat(filepath.Join(a.WorkingDir, ".gogen", "spill")); !os.IsNotExist(err) {
		t.Fatal("gate off must not create a spill dir")
	}

	// Re-enable: the same agent spills again.
	ConfigureOutputSpill(true)
	a.appendToolResult(llm.ToolCall{ID: "call_2", Name: "read_file"}, big)
	if !strings.Contains(a.Messages[1].Content, "full output saved to") {
		t.Fatalf("re-enabled gate must spill again: %q", headStr(a.Messages[1].Content, 200))
	}
}

// TestForkThenDeleteOriginalKeepsSpillRetrievable pins the fork+delete
// lifecycle end to end: a fork copies the original's spill locator lines,
// fork repoints them at the CHILD's spill dir (hardlinks), and deleting
// the ORIGINAL session afterwards removes the original's spill files —
// while the child's copies keep the full output readable.
func TestForkThenDeleteOriginalKeepsSpillRetrievable(t *testing.T) {
	// 512 bytes: comfortably above the locator length so the preview
	// carries the full locator (path + retrieval hint) plus head and tail.
	a := newSpillTestAgent(t, "fork-parent", 512)
	// Realistic transcript: user turn, assistant tool call, oversized result.
	a.appendMessage(llm.Message{Role: "user", Content: "run the big command"})
	tc := llm.ToolCall{ID: "call_1", Name: "execute_command", Args: map[string]any{"command": "big"}}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}})
	big := strings.Repeat("line-of-output\n", 100)
	a.appendToolResult(tc, big)
	if len(a.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(a.Messages))
	}
	parentPath := extractSpillPath(t, a.Messages[2].Content)
	if _, err := os.ReadFile(parentPath); err != nil {
		t.Fatalf("parent spill file unreadable: %v", err)
	}

	// Fork the whole transcript into a child session: raw index form,
	// pointing at the tool result, so the fork keeps all three messages
	// (the "last" form forks from the last assistant message and would
	// strip its tool call).
	if err := a.ForkSession(context.Background(), "2", "fork-child"); err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if a.SessionID != "fork-child" {
		t.Fatalf("SessionID = %q, want fork-child", a.SessionID)
	}
	childContent := a.Messages[2].Content
	if strings.Contains(childContent, "session-fork-parent") {
		t.Fatalf("child locator still references the parent's spill dir: %q", headStr(childContent, 300))
	}
	childPath := extractSpillPath(t, childContent)
	if filepath.Dir(childPath) != spill.NewStore(a.WorkingDir).Dir("fork-child") {
		t.Fatalf("child locator = %q, want the child's spill dir", childPath)
	}
	data, err := os.ReadFile(childPath)
	if err != nil || string(data) != big {
		t.Fatalf("child spill file = %d bytes err %v, want the full %d", len(data), err, len(big))
	}
	// The parent's file must still exist (hardlink, parent not yet deleted).
	if _, err := os.Stat(parentPath); err != nil {
		t.Fatalf("parent spill file vanished on fork: %v", err)
	}

	// THE SCENARIO: the original session is deleted → its spill files go,
	// the child's hardlinked copy keeps the full output.
	if err := spill.RemoveSessionDir(a.WorkingDir, "fork-parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parentPath); !os.IsNotExist(err) {
		t.Fatal("parent spill file survived the original's deletion")
	}
	data, err = os.ReadFile(childPath)
	if err != nil || string(data) != big {
		t.Fatalf("child retrieval broke after the original was deleted: %d bytes err %v", len(data), err)
	}
}

// TestRepointSpillLocatorsSkipsUntouchables pins the repoint scan: only
// tool-result locators pointing at real files under the spill root are
// repointed; paths outside the root, missing files, and locator text in
// non-tool messages (e.g. a model echoing the format) are left untouched.
func TestRepointSpillLocatorsSkipsUntouchables(t *testing.T) {
	a := newSpillTestAgent(t, "repoint-src", 300)
	otherPath, err := spill.NewStore(a.WorkingDir).Save("other-sess", "tool", []byte("other data"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(a.WorkingDir, "elsewhere.txt")
	missing := filepath.Join(a.WorkingDir, ".gogen", "spill", "session-missing", "gone.log")
	loc := func(p string) string { return "… total; full output saved to " + p + " — use read_file …" }
	msgs := []llm.Message{
		{Role: "user", Content: "q"},
		{Role: "tool", Content: loc(otherPath)},
		{Role: "tool", Content: loc(outside)},
		{Role: "tool", Content: loc(missing)},
		{Role: "assistant", Content: loc(otherPath)},
	}
	a.RepointSpillLocators(msgs, "repoint-dst")

	// The real locator was repointed into the target session's dir.
	if got := msgs[1].Content; strings.Contains(got, "other-sess") {
		t.Fatalf("locator not repointed: %q", got)
	}
	repointed := extractSpillPath(t, msgs[1].Content)
	if filepath.Dir(repointed) != spill.NewStore(a.WorkingDir).Dir("repoint-dst") {
		t.Fatalf("repointed path = %q, want the target session's spill dir", repointed)
	}
	data, err := os.ReadFile(repointed)
	if err != nil || string(data) != "other data" {
		t.Fatalf("repointed file = %q err %v, want the linked content", data, err)
	}
	// Untouchables keep their original text.
	if got := msgs[2].Content; !strings.Contains(got, outside) {
		t.Fatalf("path outside the spill root was rewritten: %q", got)
	}
	if got := msgs[3].Content; !strings.Contains(got, missing) {
		t.Fatalf("missing-file locator was rewritten: %q", got)
	}
	if got := msgs[4].Content; !strings.Contains(got, otherPath) {
		t.Fatalf("non-tool message was rewritten: %q", got)
	}
	// The source file is untouched by repointing.
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("source spill file vanished: %v", err)
	}
}

// TestAppendToolResultSpillsMarkerQuotingBody pins the PRECISE
// already-partial test: a body that merely CONTAINS the truncation marker
// (a file quoting it, a log line, a session snapshot) was not truncated, so
// it must still get the head + locator + tail spill preview — treating it as
// already-cut silently downgraded the result to a lossy head-only cap.
func TestAppendToolResultSpillsMarkerQuotingBody(t *testing.T) {
	a := newSpillTestAgent(t, "spill-quoted-marker", 400)
	quoted := strings.Repeat("x", 500) + "\n… truncated (quoted marker)\n" +
		strings.Repeat("y", 500) + "\nlast line"
	a.appendToolResult(llm.ToolCall{ID: "call_1", Name: "read_file"}, quoted)
	got := a.Messages[0].Content
	if len(got) > 400 {
		t.Fatalf("stored result is %d bytes, exceeds cap 400", len(got))
	}
	if !strings.Contains(got, "full output saved to") {
		t.Fatalf("marker-quoting body was not spilled: %q", headStr(got, 200))
	}
	if !strings.Contains(got, "last line") {
		t.Fatalf("spill preview lost the tail: %q", headStr(got, 200))
	}
	path := extractSpillPath(t, got)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != quoted {
		t.Fatalf("spilled content = %d bytes err %v, want the full %d", len(data), err, len(quoted))
	}
}

// TestCommandOutputWriterDiscardsUnusedSpill pins the deferred cleanup of a
// spill target nobody consumed (the cap changed to 0 between the first
// overflowing chunk and the result): the file is closed and removed instead
// of leaking on disk with its descriptor open.
func TestCommandOutputWriterDiscardsUnusedSpill(t *testing.T) {
	dir := t.TempDir()
	tgt := spill.NewStore(dir).NewTarget("sess-unused", "execute_command")
	w := newCommandOutputWriter("cmd", nil, 8, tgt)
	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	path := tgt.Path()
	if path == "" {
		t.Fatal("spill file was not opened on the overflowing chunk")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file missing after overflow: %v", err)
	}
	w.discardSpill()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unused spill file left on disk (stat err = %v)", err)
	}
	if p, _, ok := w.finishSpill(); ok || p != "" {
		t.Fatalf("target survived the discard: (%q, ok=%v)", p, ok)
	}
}

// TestCommandOutputWriterSpillFailureDisablesSpilling pins the best-effort
// contract: when the spill file cannot be opened, the writer keeps capping
// the in-memory head, never surfaces the failure to the command, and disables
// spilling for the rest of it — leaving no partial file behind.
func TestCommandOutputWriterSpillFailureDisablesSpilling(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gogen"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the spill ROOT must be: MkdirAll fails, so the
	// first overflowing chunk fails to open a file.
	if err := os.WriteFile(filepath.Join(dir, ".gogen", "spill"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgt := spill.NewStore(dir).NewTarget("sess-broken", "execute_command")
	w := newCommandOutputWriter("cmd", nil, 8, tgt)
	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatalf("writer must swallow spill errors: %v", err)
	}
	if w.spill != nil {
		t.Fatal("spill target must be disabled after a failed chunk")
	}
	if !w.overflowed || w.String() != "01234567" {
		t.Fatalf("capped head = %q overflowed=%v, want %q true", w.String(), w.overflowed, "01234567")
	}
	w.discardSpill() // no-op: the target is gone
}

// TestCapToolResultsForTurnSpillsHistoricalBody covers the normal-turn
// re-cap: a tool body that entered history oversized (e.g. a session saved
// under a larger/disabled cap, or the cap lowered live) is spilled exactly
// like a result arriving now — the full output is persisted and the inline
// body becomes a head+locator+tail preview — and the pass is sticky (a second
// pass rewrites nothing, so the prompt prefix stays stable).
func TestCapToolResultsForTurnSpillsHistoricalBody(t *testing.T) {
	a := newSpillTestAgent(t, "cap-turn-spill", 300)
	// A realistic pair: the assistant tool call (which labels the spill file)
	// and the oversized result it produced, appended straight to history so
	// the append-time cap does not touch it.
	big := strings.Repeat("historical-output\n", 100) // 1800 bytes
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_files"}}})
	a.appendMessage(llm.Message{Role: "tool", Content: big, ToolCallID: "call_1"})

	if !a.capToolResultsForTurn() {
		t.Fatal("expected the oversized historical body to be rewritten")
	}
	got := a.Messages[1].Content
	if len(got) > 300 {
		t.Fatalf("stored result is %d bytes, exceeds cap 300", len(got))
	}
	if !strings.Contains(got, "full output saved to") {
		t.Fatalf("historical body was not spilled: %q", headStr(got, 200))
	}
	if !strings.Contains(got, "historical-output") {
		t.Fatalf("preview lost the head: %q", headStr(got, 200))
	}
	path := extractSpillPath(t, got)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != big {
		t.Fatalf("spilled content = %d bytes err %v, want the full %d", len(data), err, len(big))
	}
	// The file is labeled with the tool name from the matching call.
	if !strings.Contains(filepath.Base(path), "read_files") {
		t.Fatalf("spill file %q not labeled with the tool name", filepath.Base(path))
	}
	// Sticky: a second pass must be a no-op for a stable prompt prefix.
	before := a.Messages[1].Content
	if a.capToolResultsForTurn() {
		t.Fatal("second pass rewrote a body: the cap is not sticky")
	}
	if a.Messages[1].Content != before {
		t.Fatal("second pass mutated the preview")
	}
}

// TestCapToolResultsForTurnSpillsMarkerQuotingBody pins the precise
// idempotency predicate: a body that merely CONTAINS the truncation marker
// (a file quoting it, a log line) was not truncated, so the turn pass must
// spill it exactly like a result arriving now — the coarse marker-anywhere
// check would skip it and leave it oversized. The arrival path
// (capToolResult → alreadyPartial) has always treated such a body this way;
// this keeps the historical path in agreement.
func TestCapToolResultsForTurnSpillsMarkerQuotingBody(t *testing.T) {
	a := newSpillTestAgent(t, "cap-turn-quote", 300)
	// The marker sits mid-body with content after it (so HasTruncatedTail is
	// false) and there is no locator: alreadyPartial reads "not truncated".
	big := "prefix\n… truncated (123 bytes total)\n" + strings.Repeat("quoted\n", 400)
	if alreadyPartial(big) {
		t.Fatal("premise: the marker-quoting body must not read as partial")
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_files"}}})
	a.appendMessage(llm.Message{Role: "tool", Content: big, ToolCallID: "call_1"})

	if !a.capToolResultsForTurn() {
		t.Fatal("marker-quoting body was skipped instead of spilled")
	}
	got := a.Messages[1].Content
	if !strings.Contains(got, "full output saved to") {
		t.Fatalf("marker-quoting body was not spilled: %q", headStr(got, 200))
	}
	path := extractSpillPath(t, got)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != big {
		t.Fatalf("spilled content = %d bytes err %v, want the full %d", len(data), err, len(big))
	}
	// Sticky: the preview carries an intact locator, so the next pass is a
	// no-op and the prompt prefix stays stable.
	if a.capToolResultsForTurn() {
		t.Fatal("second pass rewrote the preview: the cap is not sticky")
	}
}

// TestCapToolResultsForTurnGateOffFallsBackToPlainCap pins the fallback: with
// the spill gate off, the normal-turn re-cap takes the plain, lossy cap
// (marker, no locator, nothing written to disk) — the pre-spill behavior.
func TestCapToolResultsForTurnGateOffFallsBackToPlainCap(t *testing.T) {
	prev := OutputSpillEnabled()
	ConfigureOutputSpill(false)
	t.Cleanup(func() { ConfigureOutputSpill(prev) })

	a := newSpillTestAgent(t, "cap-turn-gate-off", 300)
	big := strings.Repeat("z", 1000)
	a.appendMessage(llm.Message{Role: "tool", Content: big, ToolCallID: "call_1"})

	if !a.capToolResultsForTurn() {
		t.Fatal("expected the oversized body to be rewritten")
	}
	got := a.Messages[0].Content
	if len(got) > 300 {
		t.Fatalf("stored result is %d bytes, exceeds cap 300", len(got))
	}
	if !contextmgr.HasTruncationMarker(got) || strings.Contains(got, "saved to") {
		t.Fatalf("gate off must use the plain cap (no locator): %q", headStr(got, 200))
	}
	if _, err := os.Stat(filepath.Join(a.WorkingDir, ".gogen", "spill")); !os.IsNotExist(err) {
		t.Fatal("gate off must not create a spill dir")
	}
}

// TestCapToolResultsForCompactStaysPlainCap pins the deliberate split: the
// forced-compaction stage-1 pass keeps the model-free, no-I/O plain cap even
// with spilling enabled (the bodies it rewrites are frequently summarized out
// on the same pass).
func TestCapToolResultsForCompactStaysPlainCap(t *testing.T) {
	a := newSpillTestAgent(t, "cap-compact-plain", 300)
	big := strings.Repeat("z", 1000)
	a.appendMessage(llm.Message{Role: "tool", Content: big, ToolCallID: "call_1"})

	if !a.capToolResultsForCompact() {
		t.Fatal("expected the oversized body to be rewritten")
	}
	got := a.Messages[0].Content
	if !contextmgr.HasTruncationMarker(got) || strings.Contains(got, "saved to") {
		t.Fatalf("compaction stage-1 must stay a plain cap: %q", headStr(got, 200))
	}
	if _, err := os.Stat(filepath.Join(a.WorkingDir, ".gogen", "spill")); !os.IsNotExist(err) {
		t.Fatal("compaction stage-1 must not write spill files")
	}
}

// TestRemoveStaleSpillsDropsUnpublishedPreview pins the cleanup for a cap
// pass the countsEpoch guard discards: a preview capToolResult just spilled
// (a fresh O_EXCL file with a random name) is removed when the rewrite is not
// published, while a plain-cap preview (no locator) is a harmless no-op.
func TestRemoveStaleSpillsDropsUnpublishedPreview(t *testing.T) {
	a := newSpillTestAgent(t, "stale-spill-test", 300)
	big := strings.Repeat("payload\n", 200) // 1600 bytes
	preview := a.capToolResult("read_files", big)
	path := extractSpillPath(t, preview)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file was not created: %v", err)
	}
	removeStaleSpills([]string{preview})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unpublished spill file %q was not removed (err %v)", path, err)
	}
	// A plain-cap preview carries no locator: nothing to remove, no panic.
	removeStaleSpills([]string{a.Context.TruncateToolResult(big)})
}
