package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/treesitter"
)

// SymbolRef represents a reference to a symbol found in a file.
type SymbolRef struct {
	File    string // Relative file path
	Line    int    // Line number (1-based)
	Content string // Line content
}

// ASTFallback is a generic helper that tries AST-based search first,
// then falls back to text-based search if AST returns no results.
// This eliminates code duplication across rename, call_graph, dependencies,
// find_definition, extract, and test_generator.
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
		return result, err // Return original error if text also fails
	}
	return textResult, nil
}

// walkSymbolReferences is a shared helper that walks the filesystem, finds symbol references
// using tree-sitter AST when available, and calls the visitor for each file with references.
// This eliminates code duplication across find_references, call_graph, and dependency_analysis.
func (e *Executor) walkSymbolReferences(ctx context.Context, searchRoot, relPrefix, glob, symbol string,
	visitor func(filePath string, refs []treesitter.Reference, content []byte) error) error {

	if !treesitter.Enabled() {
		return nil
	}

	return filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != searchRoot && shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipSearchEntry(d.Name(), false) {
			return nil
		}
		if !treesitter.ReferenceSearchSupported(path) {
			return nil
		}

		// Compute relative path with prefix
		rel, err := filepath.Rel(searchRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		}

		// Apply glob filter
		if glob != "" && !matchGlobPattern(glob, rel) {
			return nil
		}

		// Check file size and binary status
		info, err := d.Info()
		if err != nil || info.Size() > searchMaxFileBytes {
			return nil
		}
		if isBinaryFile(path) {
			return nil
		}

		// Read file and find references
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		refs, err := treesitter.FindSymbolReferences(path, content, symbol)
		if err != nil {
			if errors.Is(err, treesitter.ErrDisabled) || errors.Is(err, treesitter.ErrUnsupported) {
				return nil
			}
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
func (e *Executor) walkSymbolReferencesText(ctx context.Context, searchRoot, relPrefix, glob, symbol string,
	visitor func(filePath string, lineNum int, line string) error) error {

	pattern := fmt.Sprintf(`\b%s\b`, symbol)
	re, err := compileSearchPattern(pattern)
	if err != nil {
		return err
	}

	return filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != searchRoot && shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipSearchEntry(d.Name(), false) {
			return nil
		}

		// Check file size and binary status
		info, err := d.Info()
		if err != nil || info.Size() > searchMaxFileBytes {
			return nil
		}
		if isBinaryFile(path) {
			return nil
		}

		// Compute relative path with prefix
		rel, err := filepath.Rel(searchRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		}

		// Apply glob filter
		if glob != "" && !matchGlobPattern(glob, rel) {
			return nil
		}

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
