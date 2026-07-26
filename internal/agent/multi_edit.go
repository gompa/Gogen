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

	if files == "No matches found" {
		return "No files matched the pattern", nil
	}

	// Parse file list
	fileList := strings.Split(strings.TrimSpace(files), "\n")

	var changes []FileChange
	var totalChanges int
	var errs []string

	for _, file := range fileList {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}

		// Read raw file content (no headers/truncation)
		raw, err := e.ReadFileRawBytes(file)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: read failed: %v", file, err))
			continue
		}
		content := string(raw)

		// Count occurrences
		oldCount := strings.Count(content, search)
		if oldCount == 0 {
			continue
		}

		// Compute the result content (needed for NewCount in both paths)
		newContent := strings.ReplaceAll(content, search, replace)

		if !dryRun {
			// Apply the replacement (create or overwrite)
			if err := e.OverwriteFile(file, newContent); err != nil {
				errs = append(errs, fmt.Sprintf("%s: write failed: %v", file, err))
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

	return formatMultiEditResult(search, replace, changes, totalChanges, dryRun, errs), nil
}

func formatMultiEditResult(search, replace string, changes []FileChange, totalChanges int, dryRun bool, errs []string) string {
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

	if len(errs) > 0 {
		fmt.Fprintf(&b, "\nErrors (%d file(s)):\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}

	return b.String()
}
