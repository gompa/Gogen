package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePorcelainV2(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want GitStatus
	}{
		{
			name: "staged_only",
			in: "1 M  .M... 100644 100644 100644 " +
				"0000000000000000000000000000000000000000 83070db83f0737e87c5545b4b1a6e8b19fda3147 staged.go\n",
			want: GitStatus{
				Staged: []GitStatusEntry{{Path: "staged.go", Status: "M"}},
			},
		},
		{
			name: "new_file_staged_dot_worktree",
			in: "1 A. N... 000644 100644 100644 " +
				"0000000000000000000000000000000000000000 78981922613b2afb6025042ff6bd878ac1994e85 new_staged.go\n",
			want: GitStatus{
				Staged: []GitStatusEntry{{Path: "new_staged.go", Status: "A"}},
			},
		},
		{
			name: "unstaged_only",
			in: "1  M .M... 100644 100644 100644 " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 work.go\n",
			want: GitStatus{
				Unstaged: []GitStatusEntry{{Path: "work.go", Status: "M"}},
			},
		},
		{
			name: "partially_staged_appears_in_both",
			in: "1 MM .MM.. 100644 100644 100644 " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 both.go\n",
			want: GitStatus{
				Staged:   []GitStatusEntry{{Path: "both.go", Status: "M"}},
				Unstaged: []GitStatusEntry{{Path: "both.go", Status: "M"}},
			},
		},
		{
			// Real v2 rename record: "2 ... R100 <newPath>\t<origPath>".
			// A plain staged rename is not a conflict.
			name: "staged_rename_uses_new_path_not_conflict",
			in: "2 R. N... 100644 100644 100644 " +
				"df967b96a579e45a18b8251732d16804b2e56a55 df967b96a579e45a18b8251732d16804b2e56a55 " +
				"R100 renamed.go\ta.go\n",
			want: GitStatus{
				Staged: []GitStatusEntry{{Path: "renamed.go", Status: "R"}},
			},
		},
		{
			// v2 renames carry newPath\torigPath; spaces in either path are
			// emitted raw (unquoted), so the tab is the only separator.
			name: "staged_rename_with_worktree_mod_and_spaces",
			in: "2 RM N... 100644 100644 100644 " +
				"587be6b4c3f93f93c489c0111bba5596147a26cb b77b4eb1d946f923f61785536da9ca5af6909f06 " +
				"R50 new name.txt\tmy file.txt\n",
			want: GitStatus{
				Staged:   []GitStatusEntry{{Path: "new name.txt", Status: "R"}},
				Unstaged: []GitStatusEntry{{Path: "new name.txt", Status: "M"}},
			},
		},
		{
			// git does not quote plain spaces in paths; the path must not
			// be truncated at the first space.
			name: "path_with_raw_spaces_not_truncated",
			in: "1  M .M... 100644 100644 100644 " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 my file.go\n",
			want: GitStatus{
				Unstaged: []GitStatusEntry{{Path: "my file.go", Status: "M"}},
			},
		},
		{
			name: "quoted_path_octal_escapes",
			in:   "1 M  .M... 100644 100644 100644 0000000000000000000000000000000000000000 83070db83f0737e87c5545b4b1a6e8b19fda3147 \"\\303\\251.go\"\n",
			want: GitStatus{
				Staged: []GitStatusEntry{{Path: "é.go", Status: "M"}},
			},
		},
		{
			name: "untracked",
			in:   "? new.txt\n",
			want: GitStatus{
				Untracked: []GitStatusEntry{{Path: "new.txt", Status: "U"}},
			},
		},
		{
			name: "untracked_quoted_path_with_space",
			in:   "? \"my file.txt\"\n",
			want: GitStatus{
				Untracked: []GitStatusEntry{{Path: "my file.txt", Status: "U"}},
			},
		},
		{
			// Unmerged paths are "u" records in v2 (8 fixed fields before
			// the path), not "2" records.
			name: "unmerged_u_record",
			in: "u UU N... 100644 100644 100644 100644 " +
				"a7453f07505c42ea8d6fdda75fa91710c81c53d6 ba2906d0666cf726c7eaadd2cd3db615dedfdf3a " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 conflict.go\n",
			want: GitStatus{
				Staged:   []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
				Unstaged: []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
				Unmerged: []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
			},
		},
		{
			name: "unmerged_both_added",
			in: "u AA N... 000000 100644 100644 100644 " +
				"0000000000000000000000000000000000000000 a7453f07505c42ea8d6fdda75fa91710c81c53d6 " +
				"ba2906d0666cf726c7eaadd2cd3db615dedfdf3a c.txt\n",
			want: GitStatus{
				Staged:   []GitStatusEntry{{Path: "c.txt", Status: "A"}},
				Unstaged: []GitStatusEntry{{Path: "c.txt", Status: "A"}},
				Unmerged: []GitStatusEntry{{Path: "c.txt", Status: "U"}},
			},
		},
		{
			name: "branch_headers_with_upstream",
			in: "# branch.oid 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
				"# branch.head main\n" +
				"# branch.upstream origin/main\n" +
				"# branch.ab +2 -1\n",
			want: GitStatus{Branch: "main", Upstream: "origin/main", Ahead: 2, Behind: 1},
		},
		{
			name: "branch_headers_without_upstream_zero_ab",
			in: "# branch.oid 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
				"# branch.head main\n" +
				"# branch.ab +0 -0\n",
			want: GitStatus{Branch: "main", Ahead: 0, Behind: 0},
		},
		{
			name: "empty_output",
			in:   "",
			want: GitStatus{},
		},
		{
			name: "combined",
			in: "# branch.head main\n" +
				"# branch.ab +0 -0\n" +
				"1 M  .M... 100644 100644 100644 0000000000000000000000000000000000000000 83070db83f0737e87c5545b4b1a6e8b19fda3147 staged.go\n" +
				"1  M .M... 100644 100644 100644 83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 work.go\n" +
				"1 MM .MM.. 100644 100644 100644 83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 both.go\n" +
				"u UU N... 100644 100644 100644 100644 83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 a7453f07505c42ea8d6fdda75fa91710c81c53d6 conflict.go\n" +
				"? new.txt\n",
			want: GitStatus{
				Branch: "main",
				Staged: []GitStatusEntry{
					{Path: "staged.go", Status: "M"},
					{Path: "both.go", Status: "M"},
					{Path: "conflict.go", Status: "U"},
				},
				Unstaged: []GitStatusEntry{
					{Path: "work.go", Status: "M"},
					{Path: "both.go", Status: "M"},
					{Path: "conflict.go", Status: "U"},
				},
				Untracked: []GitStatusEntry{{Path: "new.txt", Status: "U"}},
				Unmerged:  []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePorcelainV2(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parsePorcelainV2(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestUnquoteGitPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`plain.go`, `plain.go`},
		{`"quoted.go"`, `quoted.go`},
		{`"my file.txt"`, `my file.txt`},
		{`"a\"b.go"`, `a"b.go`},
		{`"a\\b.go"`, `a\b.go`},
		{`"a\nb.go"`, "a\nb.go"},
		{`"\303\251.go"`, "é.go"},
		{`"a\tb.go"`, "a\tb.go"},
	}
	for _, tt := range tests {
		if got := unquoteGitPath(tt.in); got != tt.want {
			t.Errorf("unquoteGitPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestParsePorcelainV2Live pins parsePorcelainV2 to real git output for
// the record kinds most easily fabricated wrong: "2" (rename/copy, not a
// conflict) and "u" (the only unmerged record in porcelain v2).
func TestParsePorcelainV2Live(t *testing.T) {
	dir := newGitTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "a.go")
	gitIn(t, dir, "commit", "-m", "init")

	// Staged rename: a "2 ... newPath\torigPath" record.
	gitIn(t, dir, "mv", "a.go", "renamed.go")
	st := parsePorcelainV2(gitIn(t, dir, "status", "--porcelain=v2"))
	if len(st.Unmerged) != 0 {
		t.Fatalf("staged rename unmerged = %#v, want empty", st.Unmerged)
	}
	if !reflect.DeepEqual(st.Staged, []GitStatusEntry{{Path: "renamed.go", Status: "R"}}) {
		t.Fatalf("staged rename staged = %#v, want renamed.go/R", st.Staged)
	}

	// Real conflict: a "u" record (both sides edit a file that exists in
	// the merge base, so git reports XY=UU rather than AA).
	gitIn(t, dir, "reset", "--hard")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "c.txt")
	gitIn(t, dir, "commit", "-m", "base-c")
	head := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "c.txt")
	gitIn(t, dir, "commit", "-m", "c-main")
	gitIn(t, dir, "checkout", "-q", "HEAD~1", "-b", "feature2")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "c.txt")
	gitIn(t, dir, "commit", "-m", "c-feature")
	merge := exec.Command("git", "merge", head)
	merge.Dir = dir
	if out, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("merge unexpectedly succeeded: %s", out)
	}
	st = parsePorcelainV2(gitIn(t, dir, "status", "--porcelain=v2"))
	if !reflect.DeepEqual(st.Unmerged, []GitStatusEntry{{Path: "c.txt", Status: "U"}}) {
		t.Fatalf("conflict unmerged = %#v, want c.txt/U", st.Unmerged)
	}
	// Conflicts also surface in Staged/Unstaged so they stay actionable.
	if !reflect.DeepEqual(st.Staged, []GitStatusEntry{{Path: "c.txt", Status: "U"}}) {
		t.Fatalf("conflict staged = %#v, want c.txt/U", st.Staged)
	}
	if !reflect.DeepEqual(st.Unstaged, []GitStatusEntry{{Path: "c.txt", Status: "U"}}) {
		t.Fatalf("conflict unstaged = %#v, want c.txt/U", st.Unstaged)
	}
}
