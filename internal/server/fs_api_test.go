package server

import (
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
			name: "rename_with_companion_N_line",
			in: "1 R  .R... 100644 100644 100644 " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 old.go\n" +
				"N  .R... 100644 100644 100644 " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 old.go -> new.go\n",
			want: GitStatus{
				Staged: []GitStatusEntry{{Path: "new.go", Status: "R"}},
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
			name: "unmerged",
			in: "2 UU .U... 100644 100644 100644 " +
				"83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 conflict.go\n",
			want: GitStatus{
				Staged:   []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
				Unstaged: []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
				Unmerged: []GitStatusEntry{{Path: "conflict.go", Status: "U"}},
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
				"2 UU .U... 100644 100644 100644 83070db83f0737e87c5545b4b1a6e8b19fda3147 4b825dc642cb6eb9a060e54bf8d69288fbee4904 conflict.go\n" +
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
