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

	entries, err := os.ReadDir(searchRoot)
	if err != nil {
		return "", err
	}

	var dirs []dirCount
	var rootFiles []string
	total := 0

	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(searchRoot, name)
		if entry.IsDir() {
			if shouldSkipSearchEntry(name, true) {
				continue
			}
			n, err := countRepoFiles(path)
			if err != nil {
				return "", fmt.Errorf("%s: %w", name, err)
			}
			if n == 0 {
				continue
			}
			dirs = append(dirs, dirCount{name: name + "/", files: n})
			total += n
			continue
		}
		rootFiles = append(rootFiles, name)
		total++
	}

	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].files == dirs[j].files {
			return dirs[i].name < dirs[j].name
		}
		return dirs[i].files > dirs[j].files
	})
	sort.Strings(rootFiles)

	var b strings.Builder
	fmt.Fprintf(&b, "Repository overview (%s)\n", filepath.ToSlash(e.WorkingDir))
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

func countRepoFiles(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if walkPath != root && shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		return nil
	})
	return count, err
}
