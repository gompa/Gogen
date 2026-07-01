package treesitter

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrDisabled    = errors.New("tree-sitter is disabled (set GOGEN_TREESITTER=on or unset GOGEN_TREESITTER)")
	ErrUnsupported = errors.New("no definition query for this file type")
)

// Definition is a named symbol outline entry.
type Definition struct {
	Line int
	Kind string
	Name string
}

// ListDefinitions returns structural definitions for a supported source file.
func ListDefinitions(path string, content []byte) ([]Definition, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	return listDefinitions(path, content)
}

// FormatDefinitions renders definitions for tool output.
func FormatDefinitions(path string, defs []Definition) string {
	if len(defs) == 0 {
		return fmt.Sprintf("No definitions found in %s", path)
	}
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].Line == defs[j].Line {
			return defs[i].Name < defs[j].Name
		}
		return defs[i].Line < defs[j].Line
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Definitions in %s (%d):\n", path, len(defs))
	for _, d := range defs {
		fmt.Fprintf(&b, "L%-4d  %-10s  %s\n", d.Line, d.Kind, d.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}
