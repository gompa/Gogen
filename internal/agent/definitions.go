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
		if errors.Is(err, treesitter.ErrDisabled) {
			return "", fmt.Errorf("list_definitions requires tree-sitter (GOGEN_TREESITTER is off)")
		}
		if errors.Is(err, treesitter.ErrUnsupported) {
			return "", fmt.Errorf("no definition query for %q (supported: %s)", path, strings.Join(treesitter.DefinitionQueryLanguages(), ", "))
		}
		return "", err
	}
	return treesitter.FormatDefinitions(path, defs), nil
}
