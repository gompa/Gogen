package contextmgr

import (
	"slices"
	"testing"

	"gogen/internal/llm"
)

// TestStateResultFilesFromShowDiff pins how show_diff's touched files are
// derived. The tool's `path` argument is an optional file OR DIRECTORY (or
// absent = the whole working tree) and the implementation runs
// `git diff -- <path>`, so the argument names a SCOPE — recording it put a
// directory in the manifest's files section as if it were a file, and a path
// with no changes recorded a bogus entry for a file nothing happened to. The
// files actually shown are named by the diff itself.
func TestStateResultFilesFromShowDiff(t *testing.T) {
	tests := []struct {
		name   string
		result string
		want   []string
	}{
		{
			name: "one modified file",
			result: "diff --git a/internal/a.go b/internal/a.go\nindex 1a2b3c4..5d6e7f8 100644\n" +
				"--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-old\n+new\n",
			want: []string{"internal/a.go"},
		},
		{
			name: "a directory diff names every file",
			result: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n" +
				"diff --git a/sub/y.go b/sub/y.go\n--- a/sub/y.go\n+++ b/sub/y.go\n@@ -1 +1 @@\n-c\n+d\n" +
				"diff --git a/sub/deep/z.go b/sub/deep/z.go\n--- a/sub/deep/z.go\n+++ b/sub/deep/z.go\n",
			want: []string{"x.go", "sub/y.go", "sub/deep/z.go"},
		},
		{
			name: "a rename names both sides",
			result: "diff --git a/old/name.go b/new/name.go\nsimilarity index 100%\n" +
				"rename from old/name.go\nrename to new/name.go\n",
			want: []string{"old/name.go", "new/name.go"},
		},
		{
			name: "a new file has no old side",
			result: "diff --git a/added.go b/added.go\nnew file mode 100644\n" +
				"--- /dev/null\n+++ b/added.go\n@@ -0,0 +1 @@\n+package added\n",
			want: []string{"added.go"},
		},
		{
			name: "a deleted file has no new side",
			result: "diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n" +
				"--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package gone\n",
			want: []string{"gone.go"},
		},
		{
			name: "a quoted path with a space is one token",
			result: "diff --git \"a/my file.go\" \"b/my file.go\"\n" +
				"--- \"a/my file.go\"\n+++ \"b/my file.go\"\n",
			want: []string{"my file.go"},
		},
		{
			name:   "a C-quoted non-ASCII path is unquoted",
			result: "diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n",
			want:   []string{"caf\u00e9.go"},
		},
		{
			// git prints repository-relative paths, which for a working dir
			// below the repo root are not working-dir-relative. Kept verbatim:
			// resolving them would need a filesystem probe, and the fold must
			// depend only on the transcript.
			name:   "a path outside the working dir is kept verbatim",
			result: "diff --git a/sibling/x.go b/sibling/x.go\n--- a/sibling/x.go\n+++ b/sibling/x.go\n",
			want:   []string{"sibling/x.go"},
		},
		{
			name:   "no changes records nothing",
			result: "No differences found",
			want:   nil,
		},
		{
			name:   "empty output records nothing",
			result: "",
			want:   nil,
		},
		{
			// A plain unified diff (no `diff --git` header) names bare paths,
			// possibly with diff's trailing tab+timestamp. Git itself never
			// produces this shape, so it is pure robustness.
			name:   "a plain unified diff falls back to the ---/+++ pair",
			result: "--- only.go\t2024-01-01 00:00:00\n+++ only.go\t2024-01-01 00:00:01\n@@ -1 +1 @@\n-a\n+b\n",
			want:   []string{"only.go"},
		},
		{
			name:   "an unparsable header is skipped rather than half-parsed",
			result: "diff --git \"a/unterminated b/x.go\n--- kept.go\n+++ kept.go\n",
			want:   []string{"kept.go"},
		},
		{
			// A path that starts with a side letter is not a prefix: only the
			// side's OWN prefix is stripped, so "b/file.go" survives intact.
			name:   "a path that starts with a side letter keeps that segment",
			result: "diff --git a/b/file.go b/b/file.go\n--- a/b/file.go\n+++ b/b/file.go\n",
			want:   []string{"b/file.go"},
		},
		{
			name: "a duplicate file is recorded once",
			result: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n" +
				"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n",
			want: []string{"x.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stateResultFiles("show_diff", tc.result)
			if !ok {
				t.Fatal("stateResultFiles reported show_diff as argument-derived")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("diffStatePaths() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStateResultFilesLeavesOtherToolsOnArguments pins that only show_diff
// moved to result-derived files: every other tool still comes from the
// argument table (Fold falls back to stateTouchedFiles).
func TestStateResultFilesLeavesOtherToolsOnArguments(t *testing.T) {
	for _, tool := range []string{"read_file", "write_file", "patch_file", "git", "mcp_call"} {
		if _, ok := stateResultFiles(tool, "diff --git a/x b/x\n"); ok {
			t.Fatalf("stateResultFiles claims %q; only show_diff derives files from its result", tool)
		}
	}
}

// TestFoldRecordsTheFilesShowDiffShowed is the end-to-end half: folding a
// show_diff call over a DIRECTORY must record the files the diff showed (as
// reads) and never the directory, and an empty diff must record nothing at
// all.
func TestFoldRecordsTheFilesShowDiffShowed(t *testing.T) {
	result := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n" +
		"@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/internal/b.go b/internal/b.go\n--- a/internal/b.go\n+++ b/internal/b.go\n" +
		"@@ -1 +1 @@\n-old\n+new\n"
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "show_diff", Args: map[string]any{"path": "internal"},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: result},
	})
	if f := st.files["internal"]; f != nil {
		t.Fatalf("show_diff recorded its directory scope as a touched file: %+v", f)
	}
	for _, path := range []string{"internal/a.go", "internal/b.go"} {
		f := st.files[path]
		if f == nil {
			t.Fatalf("shown file %q missing from the files section: %+v", path, st.files)
		}
		if f.Change != "read" {
			t.Fatalf("%q classified as %q, want read (a diff inspects, it does not rewrite)", path, f.Change)
		}
		if f.Count != 1 {
			t.Fatalf("%q count = %d, want 1", path, f.Count)
		}
	}
	if len(st.files) != 2 {
		t.Fatalf("files section = %+v, want exactly the two shown files", st.files)
	}
	// The operation ledger keeps the ARGUMENT (the scope the diff was asked
	// for): that is what the model asked, and it dedupes repeat calls.
	if op := st.operations["show_diff\x00internal"]; op == nil {
		t.Fatalf("show_diff operation key missing: %+v", st.operations)
	}

	// A no-change diff records no file: the old argument table recorded the
	// path anyway, claiming the session had read a file nothing happened to.
	empty := NewSessionState()
	empty.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c2", Name: "show_diff", Args: map[string]any{"path": "internal/a.go"},
		}}},
		{Role: "tool", ToolCallID: "c2", Content: "No differences found"},
	})
	if len(empty.files) != 0 {
		t.Fatalf("a no-change diff recorded files: %+v", empty.files)
	}
	if op := empty.operations["show_diff\x00internal/a.go"]; op == nil {
		t.Fatalf("the no-change call is missing from the operation ledger: %+v", empty.operations)
	}
}
