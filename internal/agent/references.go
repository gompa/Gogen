package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gogen/internal/treesitter"
)

// FindReferences locates usages of a symbol via tree-sitter when available, otherwise word-boundary search.
func (e *Executor) FindReferences(ctx context.Context, symbol, subpath, glob string) (string, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	astMatches, astFiles, err := e.findReferencesAST(ctx, searchRoot, relPrefix, glob, symbol)
	if err != nil {
		return "", err
	}

	pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
	textOut, err := e.SearchCode(ctx, pattern, subpath, glob, 0)
	if err != nil {
		return "", err
	}

	if len(astMatches) == 0 {
		if strings.HasPrefix(textOut, "No matches found") {
			return fmt.Sprintf("No references found for %q", symbol), nil
		}
		return "References for " + symbol + " (text search):\n" + textOut, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "References for %q (%d via AST in %d files", symbol, len(astMatches), astFiles)
	if !strings.HasPrefix(textOut, "No matches found") {
		b.WriteString("; also see text search below")
	}
	b.WriteString("):\n")
	b.WriteString(strings.Join(astMatches, "\n"))

	if !strings.HasPrefix(textOut, "No matches found") {
		b.WriteString("\n\nText search (all files):\n")
		b.WriteString(textOut)
	}
	return b.String(), nil
}

func (e *Executor) findReferencesAST(ctx context.Context, searchRoot, relPrefix, glob, symbol string) ([]string, int, error) {
	if !treesitter.Enabled() {
		return nil, 0, nil
	}

	var matches []string
	astFiles := 0
	err := filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() {
			if shouldSkipSearchEntry(name, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipSearchEntry(name, false) {
			return nil
		}
		if !treesitter.ReferenceSearchSupported(path) {
			return nil
		}

		rel, err := filepath.Rel(searchRoot, path)
		if err != nil {
			return nil
		}
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		} else {
			rel = filepath.ToSlash(rel)
		}
		if glob != "" && !matchGlobPattern(glob, rel) {
			return nil
		}

		info, err := d.Info()
		if err != nil || info.Size() > searchMaxFileBytes {
			return nil
		}
		if isBinaryFile(path) {
			return nil
		}

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
		astFiles++
		matches = append(matches, treesitter.FormatReferenceMatches(rel, refs)...)
		if len(matches) >= searchMaxMatches {
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return matches, astFiles, nil
}
