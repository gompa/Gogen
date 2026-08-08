package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gogen/internal/treesitter"
)

// errReferenceLimit stops the AST reference walk once searchMaxMatches
// matches have been collected. Typed sentinel so FindReferences can
// distinguish truncation from a real walk failure with errors.Is instead of
// comparing error text.
var errReferenceLimit = errors.New("reference search: result limit reached")

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
					return errReferenceLimit
				}
				return nil
			})
		if err != nil && !errors.Is(err, errReferenceLimit) {
			return "", err
		}
	}

	var b strings.Builder
	// The AST pass already produced results — report them and skip the
	// redundant second tree walk. The text pass only runs as a fallback when
	// AST found nothing (unsupported language or no matches).
	if len(astMatches) > 0 {
		fmt.Fprintf(&b, "References for %q (%d via AST in %d files):\n", symbol, len(astMatches), astFiles)
		b.WriteString(strings.Join(astMatches, "\n"))
		return b.String(), nil
	}

	// Text search fallback
	pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
	textOut, err := e.SearchCode(ctx, pattern, subpath, glob, 0)
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(textOut, "No matches found") {
		return fmt.Sprintf("No references found for %q", symbol), nil
	}
	return "References for " + symbol + " (text search):\n" + textOut, nil
}
