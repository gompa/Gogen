package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	findFileMaxResults = 50
)

// FindFile locates files by name (exact or case-insensitive substring match).
// When limit is 0, defaults to findFileMaxResults.
func (e *Executor) FindFile(name string, subpath string, limit int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if limit <= 0 {
		limit = findFileMaxResults
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	var matches []string
	err = filepath.WalkDir(searchRoot, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if walkPath != searchRoot && shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipSearchEntry(d.Name(), false) {
			return nil
		}
		base := d.Name()
		if strings.Contains(strings.ToLower(base), strings.ToLower(name)) {
			rel, err := filepath.Rel(searchRoot, walkPath)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if relPrefix != "" {
				rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
			}
			matches = append(matches, rel)
			if len(matches) >= limit {
				return fmt.Errorf("limit reached")
			}
		}
		return nil
	})
	if err != nil && err.Error() != "limit reached" {
		return "", err
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No files found matching name %q", name), nil
	}

	sort.Strings(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Files matching %q:\n", name))
	for _, m := range matches {
		b.WriteString(m + "\n")
	}
	b.WriteString(fmt.Sprintf("\n(%d result(s))", len(matches)))
	return strings.TrimRight(b.String(), "\n"), nil
}
