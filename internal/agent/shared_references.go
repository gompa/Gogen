package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"gogen/internal/treesitter"
)

// SymbolRef represents a reference to a symbol found in a file.
type SymbolRef struct {
	File    string // Relative file path
	Line    int    // Line number (1-based)
	Content string // Line content
}

// ErrNoResults is returned by ASTFallback.Run() when both AST and text search
// fail to produce results.
var ErrNoResults = errors.New("no results found in AST or text search")

// ASTFallback is a generic helper that tries AST-based search first,
// then falls back to text-based search if AST returns no results.
// This eliminates code duplication across rename, call_graph, impact
// analysis, and find_symbol.
type ASTFallback[T any] struct {
	ASTFunc   func() (T, error) // AST-based search function
	TextFunc  func() (T, error) // Text-based fallback function
	HasResult func(T) bool      // Check if result has content
}

// Run executes AST search first, then falls back to text search if needed.
func (a *ASTFallback[T]) Run() (T, error) {
	result, err := a.ASTFunc()
	if err == nil && a.HasResult(result) {
		return result, nil
	}
	// AST failed or returned no results, try text fallback
	textResult, textErr := a.TextFunc()
	if textErr != nil {
		// Both paths failed. Return the more informative error.
		if err != nil {
			return result, err
		}
		return result, fmt.Errorf("%w: %v", ErrNoResults, textErr)
	}
	return textResult, nil
}

// walkSymbolReferences is a shared helper that walks the filesystem, finds symbol references
// using tree-sitter AST when available, and calls the visitor for each file with references.
// This eliminates code duplication across find_symbol and call_graph.
func (e *Executor) walkSymbolReferences(ctx context.Context, searchRoot, relPrefix, glob, symbol string,
	visitor func(filePath string, refs []treesitter.Reference, content []byte) error) error {

	if !treesitter.Enabled() {
		return nil
	}

	return walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true}, func(path, rel string, d os.DirEntry) error {
		if !treesitter.ReferenceSearchSupported(path) {
			return nil
		}
		// Read file and find references
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		refs, err := treesitter.FindSymbolReferences(path, content, symbol)
		if err != nil {
			return nil
		}
		if len(refs) == 0 {
			return nil
		}

		// Call the visitor with the file path and references
		return visitor(rel, refs, content)
	})
}

// walkSymbolReferencesText is a shared helper for text-based symbol search.
// It walks the filesystem and finds symbol references using regex patterns.
// The pattern parameter should be a pre-compiled regex pattern (callers use regexp.QuoteMeta).
func (e *Executor) walkSymbolReferencesText(ctx context.Context, searchRoot, relPrefix, glob, pattern string,
	visitor func(filePath string, lineNum int, line string) error) error {

	re, err := compileSearchPattern(pattern, false)
	if err != nil {
		return err
	}

	return walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true}, func(path, rel string, d os.DirEntry) error {
		// Read file and find matches
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				if err := visitor(rel, i+1, line); err != nil {
					return err
				}
			}
		}

		return nil
	})
}
