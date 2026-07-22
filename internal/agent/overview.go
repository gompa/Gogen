package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type dirCount struct {
	name  string
	files int
}

// RepoOverview summarizes top-level layout and file counts (no global index).
func (e *Executor) RepoOverview() (string, error) {
	searchRoot, _, err := e.searchRoot("")
	if err != nil {
		return "", err
	}

	// Single walk: count files per top-level directory in one pass.
	dirCounts := make(map[string]int) // top-level dir name → file count
	var rootFiles []string
	total := 0

	err = filepath.WalkDir(searchRoot, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if walkPath == searchRoot {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if shouldSkipSearchEntry(name, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipSearchEntry(name, false) {
			return nil
		}
		rel, err := filepath.Rel(searchRoot, walkPath)
		if err != nil {
			return nil
		}
		top := firstPathSegment(filepath.ToSlash(rel))
		if top == "" {
			// Root-level file
			rootFiles = append(rootFiles, name)
		} else {
			dirCounts[top]++
		}
		total++
		return nil
	})
	if err != nil {
		return "", err
	}

	var dirs []dirCount
	for name, count := range dirCounts {
		dirs = append(dirs, dirCount{name: name + "/", files: count})
	}

	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].files == dirs[j].files {
			return dirs[i].name < dirs[j].name
		}
		return dirs[i].files > dirs[j].files
	})
	sort.Strings(rootFiles)

	var b strings.Builder
	fmt.Fprintf(&b, "Repository overview (%s)\n", filepath.ToSlash(e.GetWorkingDir()))
	fmt.Fprintf(&b, "%d files under working directory (skips .git, node_modules, vendor, etc.)\n\n", total)

	if len(dirs) > 0 {
		b.WriteString("Top-level directories:\n")
		for _, d := range dirs {
			fmt.Fprintf(&b, "  %-24s %d files\n", d.name, d.files)
		}
	}
	if len(rootFiles) > 0 {
		if len(dirs) > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmt.Sprintf("Root files (%d):\n", len(rootFiles)))
		for _, f := range rootFiles {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}
	if len(dirs) == 0 && len(rootFiles) == 0 {
		b.WriteString("(empty)")
	}

	hints := []string{}
	for _, name := range []string{"README.md", "README", "GOGEN.md", "go.mod", "package.json", "pyproject.toml", "Cargo.toml"} {
		if _, err := os.Stat(filepath.Join(searchRoot, name)); err == nil {
			hints = append(hints, name)
		}
	}
	if len(hints) > 0 {
		b.WriteString("\n\nSuggested reads: " + strings.Join(hints, ", "))
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// firstPathSegment returns the first component of a slash-separated path.
func firstPathSegment(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	// No slash = root-level entry (already handled by caller).
	return ""
}
