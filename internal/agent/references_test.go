//go:build cgo

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
)

// writeReferenceFixture writes files/count .go files (f00.go, f01.go, …), each
// calling Target refsPerFile times, so the AST walk's per-file batches are
// deterministic: walkTree visits in lexical order and FormatReferenceMatches
// emits exactly one line per identifier occurrence.
func writeReferenceFixture(t *testing.T, dir string, files, refsPerFile int) {
	t.Helper()
	for i := 0; i < files; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, "package main\n\nfunc helper%02d() {\n", i)
		for j := 0; j < refsPerFile; j++ {
			sb.WriteString("\tTarget()\n")
		}
		sb.WriteString("}\n")
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.go", i)), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFindReferencesTruncation pins the AST-path limit contract: once
// searchMaxMatches accumulate, the walk stops and the result ends with the
// standard truncation footer — the "\n… truncated (" prefix that
// contextmgr/spill key on (HasTruncationMarker) — so a partial list is never
// presented as complete. Requires the real tree-sitter backend; stub builds
// have no AST pass.
func TestFindReferencesTruncation(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("AST reference walk requires CGO")
	}
	t.Setenv("GOGEN_TREESITTER", "on")

	tests := []struct {
		name        string
		files       int
		refsPerFile int
		wantHeader  string   // exact first line of the result
		wantFooter  string   // exact final line; empty means no footer expected
		wantAbsent  []string // substrings that must not appear (early-stop proof)
	}{
		{
			name:        "under limit no footer",
			files:       2,
			refsPerFile: 3,
			wantHeader:  `References for "Target" (6 via AST in 2 files):`,
			wantFooter:  "",
			wantAbsent:  []string{"… truncated ("},
		},
		{
			name:        "at limit footer marks partial list and stops the walk",
			files:       10,
			refsPerFile: 25,
			wantHeader:  `References for "Target" (200 via AST in 8 files):`,
			wantFooter:  "… truncated (showing first 200 matches; narrow with subpath/glob for more)",
			// f08/f09 hold references 201-250: the walk must have stopped at f07.
			wantAbsent: []string{"f08.go", "f09.go"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReferenceFixture(t, dir, tc.files, tc.refsPerFile)

			out, err := NewExecutor(dir).FindReferences(context.Background(), "Target", "", "*.go")
			if err != nil {
				t.Fatalf("FindReferences: %v", err)
			}
			first := out
			if i := strings.IndexByte(out, '\n'); i >= 0 {
				first = out[:i]
			}
			if first != tc.wantHeader {
				t.Fatalf("header = %q, want %q", first, tc.wantHeader)
			}
			if tc.wantFooter == "" {
				if strings.Contains(out, "… truncated (") {
					t.Fatalf("unexpected truncation footer in output: %q", out)
				}
				return
			}
			if !strings.HasSuffix(out, tc.wantFooter) {
				t.Fatalf("footer missing or misplaced, tail = %q, want suffix %q", out, tc.wantFooter)
			}
			if !contextmgr.HasTruncationMarker(out) {
				t.Fatal("footer must carry the standard upstream truncation marker (contextmgr.HasTruncationMarker)")
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(out, absent) {
					t.Fatalf("output contains %q — the walk did not stop early", absent)
				}
			}
		})
	}
}

// TestFindReferencesTextFallback pins the no-AST path: the tool delegates to
// SearchCode, whose own truncation footers (when its caps hit) stay in charge
// — the AST footer must never appear here, and a small result carries none.
func TestFindReferencesTextFallback(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER", "off")

	dir := t.TempDir()
	writeReferenceFixture(t, dir, 2, 3)

	out, err := NewExecutor(dir).FindReferences(context.Background(), "Target", "", "*.go")
	if err != nil {
		t.Fatalf("FindReferences: %v", err)
	}
	// The text header is unquoted on purpose: "References for <symbol> (text search):".
	if !strings.Contains(out, "References for Target (text search)") {
		t.Fatalf("expected text-search header, got %q", out)
	}
	if strings.Contains(out, "… truncated (") {
		t.Fatalf("no footer expected below every cap: %q", out)
	}
}
