package agent

import (
	"strings"

	"gogen/internal/treesitter"
)

// AppendSyntaxCheck appends tree-sitter syntax notes for the given paths.
func (e *Executor) AppendSyntaxCheck(result string, paths ...string) string {
	if !treesitter.Enabled() || len(paths) == 0 {
		return result
	}
	var notes []string
	for _, path := range paths {
		if strings.Contains(path, "(deleted)") || strings.TrimSpace(path) == "" {
			continue
		}
		content, err := e.readFileRaw(path)
		if err != nil {
			continue
		}
		if note := treesitter.FormatCheck(path, content); note != "" {
			notes = append(notes, note)
		}
	}
	if len(notes) == 0 {
		return result
	}
	return result + "\n\n" + strings.Join(notes, "\n\n")
}
