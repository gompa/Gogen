package contextmgr

import (
	"slices"
	"testing"

	"gogen/internal/llm"
)

// TestOperationKeyUsesTheToolSchema pins the operation-ledger dedupe key
// against each tool's REAL argument names (internal/agent/tools.go). The old
// extraction fell back to `path` first, so:
//   - every mcp_call folded to the empty key (the case was spelled "mcp", a
//     tool that does not exist — nested MCP is mcp_list/mcp_describe/mcp_call),
//   - find_symbol and read_files produced no key at all,
//   - search_code/glob/find_symbol/find_file used their directory SCOPE as the
//     key, collapsing every search in one tree into a single entry.
func TestOperationKeyUsesTheToolSchema(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{
			name: "mcp_call is keyed by the server tool",
			tool: "mcp_call",
			args: map[string]any{"tool": "ghidra_analyze", "input": map[string]any{"path": "a.bin"}},
			want: "ghidra_analyze",
		},
		{
			name: "mcp_describe is keyed by the server tool",
			tool: "mcp_describe",
			args: map[string]any{"tool": "ghidra_analyze"},
			want: "ghidra_analyze",
		},
		{
			name: "mcp_list is keyed by its query",
			tool: "mcp_list",
			args: map[string]any{"query": "ghidra"},
			want: "ghidra",
		},
		{
			name: "find_symbol is keyed by symbol, not its scope",
			tool: "find_symbol",
			args: map[string]any{"kind": "def", "symbol": "NewAgent", "path": "internal/agent"},
			want: "def NewAgent internal/agent",
		},
		{
			name: "find_symbol without a scope still has a key",
			tool: "find_symbol",
			args: map[string]any{"kind": "refs", "symbol": "foldNotice"},
			want: "refs foldNotice",
		},
		{
			name: "read_files is keyed by the list",
			tool: "read_files",
			args: map[string]any{"paths": []any{"a.go", "b.go"}},
			want: "a.go b.go",
		},
		{
			name: "read_files as a decoded string slice",
			tool: "read_files",
			args: map[string]any{"paths": []string{"c.go"}},
			want: "c.go",
		},
		{
			name: "search_code keeps the pattern",
			tool: "search_code",
			args: map[string]any{"pattern": "func main", "path": "cmd"},
			want: "func main cmd",
		},
		{
			name: "glob keeps the pattern",
			tool: "glob",
			args: map[string]any{"pattern": "**/*.go", "path": "internal"},
			want: "**/*.go internal",
		},
		{
			name: "find_file keeps the name",
			tool: "find_file",
			args: map[string]any{"name": "selection", "path": "internal/tui"},
			want: "selection internal/tui",
		},
		{
			name: "call_graph keeps the symbol",
			tool: "call_graph",
			args: map[string]any{"symbol": "renderState", "direction": "callers"},
			want: "renderState callers",
		},
		{
			name: "read_file is keyed by its path",
			tool: "read_file",
			args: map[string]any{"path": "internal/tui/selection.go"},
			want: "internal/tui/selection.go",
		},
		{
			name: "execute_command is keyed by the command",
			tool: "execute_command",
			args: map[string]any{"command": "go test ./...", "background": true},
			want: "go test ./...",
		},
		{
			name: "git keeps the action",
			tool: "git",
			args: map[string]any{"action": "log", "path": "internal", "limit": 5},
			want: "log internal",
		},
		{
			// git_stage was wired NOWHERE: every staging call folded into one
			// operation entry with an empty key, so the ledger could not say
			// what had been staged and staging a second file looked like a
			// repeat of the first.
			name: "git_stage is keyed by its path list",
			tool: "git_stage",
			args: map[string]any{"paths": []any{"a.go", "b.go"}},
			want: "a.go b.go",
		},
		{
			name: "git_stage as a decoded string slice",
			tool: "git_stage",
			args: map[string]any{"paths": []string{"c.go"}},
			want: "c.go",
		},
		{
			// "stage all" has no argument; the empty key is right here because
			// two stage-all calls really are the same operation (the operation
			// ledger keeps a count).
			name: "git_stage with no paths is one stage-all entry",
			tool: "git_stage",
			args: map[string]any{},
			want: "",
		},
		{
			name: "git_commit falls back to its message",
			tool: "git_commit",
			args: map[string]any{"message": "fix(state): dedupe"},
			want: "fix(state): dedupe",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationKey(tc.tool, tc.args); got != tc.want {
				t.Fatalf("operationKey(%q, %v) = %q, want %q", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

// TestStateTouchedFilesSkipsDirectoryScopes pins which calls enter the
// manifest's files section. A directory SCOPE is not a touched file: the
// section answers "what did I change" and "what did I already read", and
// recording "internal/agent" as a file claimed the session had opened a
// directory (and made an edit tool's scope look like a modified file).
func TestStateTouchedFilesSkipsDirectoryScopes(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want []string
	}{
		{name: "read_file names a file", tool: "read_file", args: map[string]any{"path": "a.go"}, want: []string{"a.go"}},
		{name: "replace_in_file names a file", tool: "replace_in_file", args: map[string]any{"path": "a.go", "search": "x", "replace": "y"}, want: []string{"a.go"}},
		{name: "delete names a path", tool: "delete", args: map[string]any{"path": "gone.go"}, want: []string{"gone.go"}},
		{name: "git_blame names a file", tool: "git_blame", args: map[string]any{"file": "a.go"}, want: []string{"a.go"}},
		{
			name: "read_files names several files",
			tool: "read_files",
			args: map[string]any{"paths": []any{"a.go", "b.go"}},
			want: []string{"a.go", "b.go"},
		},
		{name: "search_code scope is not a file", tool: "search_code", args: map[string]any{"pattern": "x", "path": "internal/agent"}, want: nil},
		{name: "glob scope is not a file", tool: "glob", args: map[string]any{"pattern": "*.go", "path": "internal"}, want: nil},
		{name: "find_symbol scope is not a file", tool: "find_symbol", args: map[string]any{"symbol": "x", "path": "internal"}, want: nil},
		{name: "find_file scope is not a file", tool: "find_file", args: map[string]any{"name": "x", "path": "internal"}, want: nil},
		{name: "list_files root is not a file", tool: "list_files", args: map[string]any{"path": "."}, want: nil},
		{name: "call_graph scope is not a file", tool: "call_graph", args: map[string]any{"symbol": "x", "path": "internal"}, want: nil},
		{name: "rename_symbol scope is not a file", tool: "rename_symbol", args: map[string]any{"old_name": "a", "new_name": "b", "path": "internal"}, want: nil},
		{name: "git scope is not a file", tool: "git", args: map[string]any{"action": "log", "path": "internal"}, want: nil},
		{name: "repo_overview touches nothing", tool: "repo_overview", args: map[string]any{}, want: nil},
		// A foreign (MCP) tool keeps the historical guess: its schema is
		// unknown here, and an MCP tool named "read_file" does take a path.
		{name: "unknown tool keeps the path guess", tool: "acme_read", args: map[string]any{"path": "a.go"}, want: []string{"a.go"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stateTouchedFiles(tc.tool, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("stateTouchedFiles(%q, %v) = %v, want %v", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

// TestFoldSkipsScopeAsFile is the end-to-end half: folding a search with a
// directory scope must not add a files entry, while the same session's edit
// does — and both keep their own operation keys.
func TestFoldSkipsScopeAsFile(t *testing.T) {
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "search_code", Args: map[string]any{"pattern": "TODO", "path": "internal/tui"}},
			{ID: "c2", Name: "replace_in_file", Args: map[string]any{"path": "internal/tui/selection.go", "search": "a", "replace": "b"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "internal/tui/x.go:1:TODO"},
		{Role: "tool", ToolCallID: "c2", Content: "Replaced 1 occurrence"},
	})
	if f := st.files["internal/tui"]; f != nil {
		t.Fatalf("a directory scope was recorded as a touched file: %+v", f)
	}
	if f := st.files["internal/tui/selection.go"]; f == nil || f.Change != "modified" {
		t.Fatalf("the edited file is not recorded as modified: %+v", f)
	}
	if op := st.operations["search_code\x00TODO internal/tui"]; op == nil {
		t.Fatalf("search operation missing its pattern key: %+v", st.operations)
	}
}

// TestFoldGitStageKeepsPerPathKeysAndNoFileEntry pins the two decisions behind
// the git_stage wiring:
//
//  1. staging two different files is two ledger entries (before the wiring
//     every call folded into ONE entry with an empty key, so "what did I
//     stage?" was unanswerable and the second call looked like a repeat);
//  2. staging records NO files entry: it changes the index, not the working
//     tree, so "modified" would be false and "read" would claim the model had
//     seen the file's content. A file the session already edited stays
//     modified — staging it must not downgrade the entry.
func TestFoldGitStageKeepsPerPathKeysAndNoFileEntry(t *testing.T) {
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "replace_in_file", Args: map[string]any{"path": "a.go", "search": "x", "replace": "y"}},
			{ID: "c2", Name: "git_stage", Args: map[string]any{"paths": []any{"a.go"}}},
			{ID: "c3", Name: "git_stage", Args: map[string]any{"paths": []any{"b.go"}}},
			{ID: "c4", Name: "git_stage", Args: map[string]any{}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "Replaced 1 occurrence"},
		{Role: "tool", ToolCallID: "c2", Content: "Staged 1 file"},
		{Role: "tool", ToolCallID: "c3", Content: "Staged 1 file"},
		{Role: "tool", ToolCallID: "c4", Content: "Staged all changes"},
	})

	for _, key := range []string{"a.go", "b.go", ""} {
		if op := st.operations["git_stage\x00"+key]; op == nil {
			t.Fatalf("git_stage entry for key %q missing: %+v", key, st.operations)
		}
	}
	if f := st.files["b.go"]; f != nil {
		t.Fatalf("staging recorded %+v; a stage changes the index, not the working tree", f)
	}
	if f := st.files["a.go"]; f == nil || f.Change != "modified" {
		t.Fatalf("staging downgraded the already-edited file's entry: %+v", f)
	}
	if len(st.files) != 1 {
		t.Fatalf("files section = %+v, want only the edited file", st.files)
	}
}
