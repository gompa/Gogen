package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gogen/internal/treesitter"
)

type RenameResult struct {
	FilesModified int
	Changes       []FileChange
}

type FileChange struct {
	Path         string
	LinesChanged []int
	Count        int
	OldCount     int
	NewCount     int
}

// RenameSymbol renames a symbol across all files.
// Uses tree-sitter for supported languages, falls back to word-boundary text search.
func (e *Executor) RenameSymbol(ctx context.Context, oldName, newName, subpath, glob string, dryRun bool) (string, error) {
	if oldName == "" || newName == "" {
		return "", fmt.Errorf("old_name and new_name are required")
	}
	if oldName == newName {
		return "", fmt.Errorf("old_name and new_name are the same")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	var changes []FileChange

	// Try tree-sitter first for supported languages
	if treesitter.Enabled() {
		changes, err = e.renameWithAST(ctx, searchRoot, relPrefix, glob, oldName, newName, dryRun)
		if err == nil && len(changes) > 0 {
			return formatRenameResult(oldName, newName, changes, dryRun), nil
		}
	}

	// Fallback: word-boundary text search (works for all languages)
	changes, err = e.renameWithText(ctx, searchRoot, relPrefix, glob, oldName, newName, dryRun)
	if err != nil {
		return "", err
	}

	return formatRenameResult(oldName, newName, changes, dryRun), nil
}

func (e *Executor) renameWithAST(ctx context.Context, searchRoot, relPrefix, glob, oldName, newName string, dryRun bool) ([]FileChange, error) {
	var changes []FileChange

	err := filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || shouldSkipSearchEntry(d.Name(), d.IsDir()) {
			return nil
		}

		if !treesitter.ReferenceSearchSupported(path) {
			return nil
		}

		// Apply glob filter if specified
		if glob != "" {
			rel, _ := filepath.Rel(searchRoot, path)
			rel = filepath.ToSlash(rel)
			if !matchGlobPattern(glob, rel) {
				return nil
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		refs, err := treesitter.FindSymbolReferences(path, content, oldName)
		if err != nil || len(refs) == 0 {
			return nil
		}

		// Apply renames
		newContent := string(content)
		var linesChanged []int

		for _, ref := range refs {
			// Replace the symbol at the exact line
			lines := strings.Split(newContent, "\n")
			if ref.Line-1 < len(lines) {
				oldLine := lines[ref.Line-1]
				// Use word-boundary replacement to avoid partial matches
				re := regexp.MustCompile(`\b` + regexp.QuoteMeta(oldName) + `\b`)
				lines[ref.Line-1] = re.ReplaceAllString(oldLine, newName)
				newContent = strings.Join(lines, "\n")
				linesChanged = append(linesChanged, ref.Line)
			}
		}

		if !dryRun {
			if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
				return err
			}
		}

		rel, _ := filepath.Rel(searchRoot, path)
		changes = append(changes, FileChange{
			Path:         filepath.ToSlash(filepath.Join(relPrefix, rel)),
			LinesChanged: linesChanged,
			Count:        len(refs),
		})

		return nil
	})

	return changes, err
}

func (e *Executor) renameWithText(ctx context.Context, searchRoot, relPrefix, glob, oldName, newName string, dryRun bool) ([]FileChange, error) {
	// Word-boundary pattern for text fallback
	pattern := `\b` + regexp.QuoteMeta(oldName) + `\b`
	re := regexp.MustCompile(pattern)

	var changes []FileChange

	err := filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || shouldSkipSearchEntry(d.Name(), d.IsDir()) {
			return nil
		}

		if isBinaryFile(path) {
			return nil
		}

		// Apply glob filter if specified
		if glob != "" {
			rel, _ := filepath.Rel(searchRoot, path)
			rel = filepath.ToSlash(rel)
			if !matchGlobPattern(glob, rel) {
				return nil
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Check for matches
		matches := re.FindAllIndex(content, -1)
		if len(matches) == 0 {
			return nil
		}

		// Apply replacement
		newContent := re.ReplaceAll(content, []byte(newName))

		if !dryRun {
			if err := os.WriteFile(path, newContent, 0o644); err != nil {
				return err
			}
		}

		// Track which lines changed
		var linesChanged []int
		oldLines := strings.Split(string(content), "\n")
		newLines := strings.Split(string(newContent), "\n")
		for i, line := range oldLines {
			if i < len(newLines) && line != newLines[i] {
				linesChanged = append(linesChanged, i+1)
			}
		}

		rel, _ := filepath.Rel(searchRoot, path)
		changes = append(changes, FileChange{
			Path:         filepath.ToSlash(filepath.Join(relPrefix, rel)),
			LinesChanged: linesChanged,
			Count:        len(matches),
		})

		return nil
	})

	return changes, err
}

func formatRenameResult(oldName, newName string, changes []FileChange, dryRun bool) string {
	var b strings.Builder
	action := "Renamed"
	if dryRun {
		action = "Would rename"
	}

	total := 0
	for _, c := range changes {
		total += c.Count
	}

	fmt.Fprintf(&b, "%s %d occurrence(s) of %q -> %q in %d file(s):\n\n",
		action, total, oldName, newName, len(changes))

	for _, c := range changes {
		fmt.Fprintf(&b, "  %s (lines: %v)\n", c.Path, c.LinesChanged)
	}

	return b.String()
}
