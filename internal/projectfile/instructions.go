package projectfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Instruction byte budgets for AGENTS.md/CLAUDE.md content. Caps keep the
// appended instructions bounded so enabling the feature can never blow up
// the system prompt.
const (
	// MaxInstructionFileBytes is the per-file cap: a larger instruction
	// file is skipped entirely (never truncated).
	MaxInstructionFileBytes = 32 * 1024
	// MaxInstructionTotalBytes is the aggregate cap: discovery stops
	// collecting once the rendered total would exceed it.
	MaxInstructionTotalBytes = 64 * 1024
)

// InstructionFile is one discovered workspace instruction file (AGENTS.md or
// CLAUDE.md) with its trimmed body.
type InstructionFile struct {
	// Path is the absolute file path (also used as the rendered header).
	Path string
	// Content is the trimmed markdown body.
	Content string
}

// hasVCSMarker reports whether dir contains a .git or .hg marker (a
// directory, or a file for git worktrees). It identifies the project root
// where the upward instruction walk stops.
func hasVCSMarker(dir string) bool {
	for _, name := range []string{".git", ".hg"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// DiscoverInstructions walks up from workingDir to the project root and
// collects AGENTS.md then CLAUDE.md per directory, nearest directory first.
//
// The walk stops at the first directory containing a .git/.hg marker (that
// directory's own files are included), at the user's home directory (its
// files are NOT read), or at the filesystem root. Duplicate trimmed content
// is collapsed to its first occurrence; files over the per-file cap are
// skipped; collection stops once the total cap is reached. Missing roots and
// unreadable files are skipped — never an error.
func DiscoverInstructions(workingDir string) ([]InstructionFile, error) {
	dir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, err
	}
	home := HomeDir()

	var files []InstructionFile
	seen := make(map[string]struct{})
	total := 0
	for {
		// Never read instruction files from the home directory itself:
		// the walk covers project directories below it only.
		if home != "" && home != "." && dir == home {
			break
		}
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			if total >= MaxInstructionTotalBytes {
				return files, nil
			}
			p := filepath.Join(dir, name)
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if len(data) > MaxInstructionFileBytes {
				continue
			}
			body := strings.TrimSpace(string(data))
			if body == "" {
				continue
			}
			if _, dup := seen[body]; dup {
				continue
			}
			// Never exceed the total cap: a file that would push the
			// aggregate over the budget ends discovery (the earlier files
			// stay).
			if total+len(body) > MaxInstructionTotalBytes {
				return files, nil
			}
			seen[body] = struct{}{}
			files = append(files, InstructionFile{Path: p, Content: body})
			total += len(body)
		}
		if hasVCSMarker(dir) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root
		}
		dir = parent
	}
	return files, nil
}

// RenderInstructions renders discovered instruction files as markdown
// sections with per-file headers so the model can attribute each rule to
// its source file. Returns "" for no files.
func RenderInstructions(files []InstructionFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "\n## From %s\n\n%s\n", f.Path, f.Content)
	}
	return strings.TrimSpace(b.String())
}

// LoadInstructions discovers and renders workspace instruction files.
// Returns "" (never an error) when no instruction files exist.
func LoadInstructions(workingDir string) (string, error) {
	files, err := DiscoverInstructions(workingDir)
	if err != nil {
		return "", err
	}
	return RenderInstructions(files), nil
}
