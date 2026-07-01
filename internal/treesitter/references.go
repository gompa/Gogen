package treesitter

import (
	"fmt"
	"sort"
	"strings"
)

// Reference is a symbol usage location in a source file.
type Reference struct {
	Line int
	Text string
}

// FindSymbolReferences locates identifier occurrences matching symbol in a source file.
func FindSymbolReferences(path string, content []byte, symbol string) ([]Reference, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	return findSymbolReferences(path, content, symbol)
}

// FormatReferenceMatches renders references in search_code-style lines.
func FormatReferenceMatches(relPath string, refs []Reference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s:%d:%s", relPath, r.Line, r.Text))
	}
	return out
}

func sortReferences(refs []Reference) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Line < refs[j].Line
	})
}

func dedupeReferences(refs []Reference) []Reference {
	seen := make(map[int]struct{}, len(refs))
	out := make([]Reference, 0, len(refs))
	for _, r := range refs {
		if _, ok := seen[r.Line]; ok {
			continue
		}
		seen[r.Line] = struct{}{}
		out = append(out, r)
	}
	return out
}

func lineTextAt(content []byte, line int) string {
	if line < 1 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if line > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[line-1])
}
