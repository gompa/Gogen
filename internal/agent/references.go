package agent

import (
	"context"
	"fmt"
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

	// Collect AST matches using shared helper
	var astMatches []string
	astFiles := 0
	if treesitter.Enabled() {
		err = e.walkSymbolReferences(ctx, searchRoot, relPrefix, glob, symbol,
			func(filePath string, refs []treesitter.Reference, content []byte) error {
				astFiles++
				astMatches = append(astMatches, treesitter.FormatReferenceMatches(filePath, refs)...)
				if len(astMatches) >= searchMaxMatches {
					return fmt.Errorf("limit reached")
				}
				return nil
			})
		if err != nil && err.Error() != "limit reached" {
			return "", err
		}
	}

	// Text search fallback
	pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
	textOut, err := e.SearchCode(ctx, pattern, subpath, glob, 0)
	if err != nil {
		return "", err
	}

	// Format output
	var b strings.Builder
	if len(astMatches) > 0 {
		fmt.Fprintf(&b, "References for %q (%d via AST in %d files", symbol, len(astMatches), astFiles)
		if !strings.HasPrefix(textOut, "No matches found") {
			b.WriteString("; also see text search below")
		}
		b.WriteString("):\n")
		b.WriteString(strings.Join(astMatches, "\n"))
	} else if strings.HasPrefix(textOut, "No matches found") {
		return fmt.Sprintf("No references found for %q", symbol), nil
	} else {
		b.WriteString("References for " + symbol + " (text search):\n")
		b.WriteString(textOut)
	}

	if !strings.HasPrefix(textOut, "No matches found") && len(astMatches) > 0 {
		b.WriteString("\n\nText search (all files):\n")
		b.WriteString(textOut)
	}
	return b.String(), nil
}
