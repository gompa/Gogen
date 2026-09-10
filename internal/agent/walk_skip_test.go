package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeUnreadableDir creates an unreadable subdirectory "locked" holding a file
// and returns a cleanup that restores permissions (so t.TempDir can remove it).
// It skips the test on platforms/situations where permission bits are not
// enforced (Windows, root, or ACL-less filesystems) so the assertion is not
// flaky.
func makeUnreadableDir(t *testing.T, dir string) {
	t.Helper()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "secret.txt"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	// Probe: if the directory is still readable after chmod (root, some
	// filesystems), the test cannot exercise the error path.
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("permission bits are not enforced here; cannot exercise walk errors")
	}
}

// TestWalkTreeSurfacesSkippedDirs is the regression guard for the silently
// swallowed walk error: an unreadable directory must be reported as a skipped
// path (not indistinguishable from an empty dir) while the rest of the tree is
// still walked.
func TestWalkTreeSurfacesSkippedDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeUnreadableDir(t, dir)

	exec := NewExecutor(dir)
	out, err := exec.ListFiles(context.Background(), ".", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok.txt") {
		t.Fatalf("readable file omitted from listing: %q", out)
	}
	// The reported path must be workspace-relative ("locked"), not the
	// absolute temp-dir path (which would leak the workspace location).
	if !strings.Contains(out, "skipped 1 unreadable path (locked:") {
		t.Fatalf("unreadable dir not surfaced as a workspace-relative skip: %q", out)
	}
}

// TestWalkSkipsFooter pins the footer rendering (singular/plural, reason
// extraction) without touching the filesystem.
func TestWalkSkipsFooter(t *testing.T) {
	if got := (*walkSkips)(nil).footer(); got != "" {
		t.Fatalf("nil skips footer = %q, want empty", got)
	}
	if got := (&walkSkips{}).footer(); got != "" {
		t.Fatalf("empty skips footer = %q, want empty", got)
	}
	var w walkSkips
	w.observe("a/b", os.ErrPermission)
	if got := w.footer(); !strings.Contains(got, "1 unreadable path") || !strings.Contains(got, "a/b") {
		t.Fatalf("single-skip footer = %q", got)
	}
	w.observe("c", os.ErrNotExist)
	if got := w.footer(); !strings.Contains(got, "2 unreadable paths") || !strings.Contains(got, "first: a/b") {
		t.Fatalf("multi-skip footer = %q", got)
	}
}
