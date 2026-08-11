package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPatchFixtures runs every fixture under testdata/patches through the
// real parseUnifiedDiff + applyPatchHunks pipeline and asserts the resulting
// file contents. The fixtures capture real model-generated diff shapes (CRLF
// and bare-CR input, duplicate ---/+++ headers, SQL comment lines starting
// "-- ", declared hunk-count drift over/under, /dev/null new and deleted
// files, rename sections, "\ No newline at end of file" markers, multi-file
// patches, and trailing garbage the model added). This corpus is the
// regression net for the parseUnifiedDiff state-machine refactor and any
// future LLM-output parsing change.
//
// Layout: testdata/patches/<name>/patch.diff plus orig/<path> (pre-image
// contents, one per --- header, path without the a/ prefix) and want/<path>
// (post-image contents, path without the b/ prefix). A newName of /dev/null
// asserts the file is reduced to zero lines; an oldName of /dev/null asserts
// the insertion applies onto an empty file.
func TestPatchFixtures(t *testing.T) {
	dirs, err := filepath.Glob("testdata/patches/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no fixture directories under testdata/patches")
	}
	for _, dir := range dirs {
		dir := dir
		t.Run(filepath.Base(dir), func(t *testing.T) {
			diffB, err := os.ReadFile(filepath.Join(dir, "patch.diff"))
			if err != nil {
				t.Fatal(err)
			}
			files, err := parseUnifiedDiff(string(diffB))
			if err != nil {
				t.Fatalf("parseUnifiedDiff: %v", err)
			}
			if len(files) == 0 {
				t.Fatal("parseUnifiedDiff returned no file sections")
			}
			for _, pf := range files {
				oldPath := strings.TrimPrefix(pf.oldName, "a/")
				newPath := strings.TrimPrefix(pf.newName, "b/")
				var orig []string
				if oldPath != "/dev/null" {
					origB, err := os.ReadFile(filepath.Join(dir, "orig", filepath.FromSlash(oldPath)))
					if err != nil {
						t.Fatalf("orig/%s: %v", oldPath, err)
					}
					orig = splitLinesPreserveTrailing(string(origB))
				}
				got, _, err := applyPatchHunks(orig, pf.hunks, true)
				if err != nil {
					t.Fatalf("%s: applyPatchHunks: %v", pf.newName, err)
				}
				if newPath == "/dev/null" {
					if len(got) != 0 {
						t.Fatalf("%s: expected deletion (0 lines), got %d lines", pf.oldName, len(got))
					}
					continue
				}
				wantB, err := os.ReadFile(filepath.Join(dir, "want", filepath.FromSlash(newPath)))
				if err != nil {
					t.Fatalf("want/%s: %v", newPath, err)
				}
				want := splitLinesPreserveTrailing(string(wantB))
				if len(got) != len(want) {
					t.Fatalf("%s: got %d lines, want %d:\n got: %q\nwant: %q", newPath, len(got), len(want), got, want)
				}
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("%s: line %d: got %q, want %q", newPath, i+1, got[i], want[i])
					}
				}
			}
		})
	}
}
