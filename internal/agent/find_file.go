package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// errFindFileLimit signals that FindFile stopped the walk because it reached
// the result limit. Kept as a sentinel (rather than an ad-hoc error string)
// so callers can distinguish truncation from a real walk failure.
var errFindFileLimit = errors.New("find_file: result limit reached")

const (
	findFileMaxResults = 50
)

// FindFile locates files by name (exact or case-insensitive substring match).
// When limit is 0, defaults to findFileMaxResults.
// ctx is threaded into the tree walk so cancellation aborts the scan.
func (e *Executor) FindFile(ctx context.Context, name string, subpath string, limit int) (string, error) {
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
	err = walkTree(ctx, searchRoot, relPrefix, walkOpts{}, func(path, rel string, d os.DirEntry) error {
		base := d.Name()
		if strings.Contains(strings.ToLower(base), strings.ToLower(name)) {
			matches = append(matches, rel)
			if len(matches) >= limit {
				return errFindFileLimit
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFindFileLimit) {
		return "", err
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No files found matching name %q", name), nil
	}

	slices.Sort(matches)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Files matching %q:\n", name))
	for _, m := range matches {
		b.WriteString(m + "\n")
	}
	b.WriteString(fmt.Sprintf("\n(%d result(s))", len(matches)))
	return strings.TrimRight(b.String(), "\n"), nil
}
