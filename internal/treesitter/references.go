package treesitter

import (
	"fmt"
	"sort"
	"strings"
)

// Reference is a symbol usage location in a source file. Start and End are
// byte offsets of the identifier within the file, allowing callers to
// replace exactly the identifier span (a string literal or comment on the
// same line must not be renamed). They are zero when the backend cannot
// provide them (stub builds).
type Reference struct {
	Line  int
	Start int
	End   int
	Text  string
}

// FindSymbolReferences locates identifier occurrences matching symbol in a source file.
func FindSymbolReferences(path string, content []byte, symbol string) ([]Reference, error) {
	if err := guardEnabled(); err != nil {
		return nil, err
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
		if refs[i].Start != refs[j].Start {
			return refs[i].Start < refs[j].Start
		}
		return refs[i].End < refs[j].End
	})
}

func dedupeReferences(refs []Reference) []Reference {
	seen := make(map[[2]int]struct{}, len(refs))
	out := make([]Reference, 0, len(refs))
	for _, r := range refs {
		key := [2]int{r.Start, r.End}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
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
