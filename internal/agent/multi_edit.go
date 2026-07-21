package agent

import (
	"context"
	"fmt"
	"strings"
)

type MultiEditResult struct {
	FilesModified int
	TotalChanges  int
	Changes       []FileChange
	DryRun        bool
}

// MultiEdit applies the same transformation across multiple files.
// Language-agnostic - works with any text file.
func (e *Executor) MultiEdit(ctx context.Context, pattern, search, replace string, dryRun bool) (string, error) {
	if search == "" {
		return "", fmt.Errorf("search string is required")
	}

	// Find all files matching the glob pattern
	files, err := e.GlobFiles(pattern, "", false)
	if err != nil {
		return "", err
	}

	if files == "No files found" {
		return "No files matched the pattern", nil
	}

	// Parse file list
	fileList := strings.Split(strings.TrimSpace(files), "\n")

	var changes []FileChange
	var totalChanges int

	for _, file := range fileList {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}

		// Read file content
		content, err := e.ReadFileRange(file, 0, 0, "", false)
		if err != nil {
			continue // Skip files that can't be read
		}

		// Count occurrences
		oldCount := strings.Count(content, search)
		if oldCount == 0 {
			continue
		}

		// Compute the result content (needed for NewCount in both paths)
		newContent := strings.ReplaceAll(content, search, replace)

		if !dryRun {
			// Apply the replacement
			if err := e.WriteFile(file, newContent); err != nil {
				continue
			}
		}

		// Track changes
		changes = append(changes, FileChange{
			Path:     file,
			OldCount: oldCount,
			NewCount: strings.Count(newContent, search),
		})
		totalChanges += oldCount
	}

	return formatMultiEditResult(search, replace, changes, totalChanges, dryRun), nil
}

func formatMultiEditResult(search, replace string, changes []FileChange, totalChanges int, dryRun bool) string {
	var b strings.Builder

	action := "Replaced"
	if dryRun {
		action = "Would replace"
	}

	fmt.Fprintf(&b, "%s %d occurrence(s) of %q with %q in %d file(s):\n\n",
		action, totalChanges, search, replace, len(changes))

	for _, c := range changes {
		fmt.Fprintf(&b, "  %s (%d occurrence(s))\n", c.Path, c.OldCount)
	}

	return b.String()
}
