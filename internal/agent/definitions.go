package agent

import (
	"errors"
	"fmt"
	"strings"

	"gogen/internal/treesitter"
)

// ListDefinitions returns an outline of named symbols in a source file.
func (e *Executor) ListDefinitions(path string) (string, error) {
	content, err := e.ReadFileRawBytes(path)
	if err != nil {
		return "", err
	}
	defs, err := treesitter.ListDefinitions(path, content)
	if err != nil {
		// Graceful degradation: when tree-sitter is disabled or has no query
		// for this file type, fall back to a line-based text outline instead
		// of erroring, so the tool stays useful in every build.
		if errors.Is(err, treesitter.ErrDisabled) || errors.Is(err, treesitter.ErrUnsupported) {
			return listDefinitionsText(path, content), nil
		}
		return "", err
	}
	return treesitter.FormatDefinitions(path, defs), nil
}

// listDefinitionsMax caps the text-outline size, mirroring the tree-sitter
// path's maxDefinitions cap so huge files cannot flood the conversation.
const listDefinitionsMax = 300

// listDefinitionsText is the text-fallback outline for list_definitions. It
// mirrors the AST output shape (line, kind, name) using the shared
// classifyDefinitionLine scanner, capped like the AST path.
func listDefinitionsText(path string, content []byte) string {
	type outline struct {
		line int
		kind string
		name string
	}
	var out []outline
	for i, line := range strings.Split(string(content), "\n") {
		kind, name, _, ok := classifyDefinitionLine(line)
		if !ok {
			continue
		}
		out = append(out, outline{line: i + 1, kind: kind, name: name})
		if len(out) >= listDefinitionsMax {
			break
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("No definitions found in %s", path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Definitions in %s (%d, text outline):\n", path, len(out))
	for _, d := range out {
		fmt.Fprintf(&b, "L%-4d  %-10s  %s\n", d.line, d.kind, d.name)
	}
	return strings.TrimRight(b.String(), "\n")
}
