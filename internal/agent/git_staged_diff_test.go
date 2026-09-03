package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitStagedDiff(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, dir string)
		wantErr   bool
		wantEmpty bool
		wantSub   string
	}{
		{
			name: "staged change after a commit",
			setup: func(t *testing.T, dir string) {
				gitInit(t, dir)
				commitFile(t, dir, "a.txt", "old\n", "initial")
				if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGitCmd(t, dir, "add", "a.txt")
			},
			wantSub: "+new",
		},
		{
			name: "unborn HEAD diffs against the empty tree",
			setup: func(t *testing.T, dir string) {
				gitInit(t, dir)
				if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("hello\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGitCmd(t, dir, "add", "first.txt")
			},
			wantSub: "+hello",
		},
		{
			name: "nothing staged returns empty",
			setup: func(t *testing.T, dir string) {
				gitInit(t, dir)
				commitFile(t, dir, "a.txt", "old\n", "initial")
			},
			wantEmpty: true,
		},
		{
			name: "not a git repository",
			setup: func(t *testing.T, dir string) {
				// No git init: the diff command must surface an error.
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			out, err := NewExecutor(dir).GitStagedDiff(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("GitStagedDiff: %v", err)
			}
			if tt.wantEmpty {
				if out != "" {
					t.Fatalf("expected empty diff, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tt.wantSub) {
				t.Fatalf("diff missing %q: %q", tt.wantSub, out)
			}
		})
	}
}
