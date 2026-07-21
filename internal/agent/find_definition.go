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

// FindDefinition locates the file and line where a symbol is defined.
// Uses tree-sitter AST when available for supported languages; falls back to text search.
func (e *Executor) FindDefinition(ctx context.Context, symbol, subpath, glob string) (string, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	// Use AST fallback pattern: try AST first, then text search
	fallback := &ASTFallback[[]string]{
		ASTFunc: func() ([]string, error) {
			return e.findDefinitionAST(ctx, searchRoot, relPrefix, glob, symbol)
		},
		TextFunc: func() ([]string, error) {
			return e.findDefinitionText(ctx, subpath, glob, symbol)
		},
		HasResult: func(defs []string) bool {
			return len(defs) > 0
		},
	}

	defs, err := fallback.Run()
	if err != nil {
		return "", err
	}

	if len(defs) == 0 {
		return fmt.Sprintf("No definition found for %q", symbol), nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Definition(s) for %q:\n", symbol))
	for _, def := range defs {
		b.WriteString(def + "\n")
	}
	b.WriteString(fmt.Sprintf("\n(%d definition(s) found)", len(defs)))
	return b.String(), nil
}

// findDefinitionText performs text-based search for symbol definitions.
func (e *Executor) findDefinitionText(ctx context.Context, subpath, glob, symbol string) ([]string, error) {
	// Combine all definition patterns into a single alternation to avoid
	// spawning ripgrep (or walking the tree) 8 separate times.
	q := regexp.QuoteMeta(symbol)
	pattern := `(?m)(?:` +
		`\bfunc\s+` + q + `\s*\(` + `|` +
		`\bfunc\s*\([^)]*\)\s+` + q + `\s*\(` + `|` +
		`\btype\s+` + q + `\s+(struct|interface|func|type)` + `|` +
		`\bvar\s+` + q + `\b` + `|` +
		`\bconst\s+` + q + `\b` + `|` +
		`\blet\s+` + q + `\b` + `|` +
		`\bclass\s+` + q + `\b` + `|` +
		`\bdef\s+` + q + `\s*\(` +
		`)`

	out, err := e.SearchCode(ctx, pattern, subpath, glob, 0)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(out, "No matches") {
		return nil, nil
	}

	var allDefs []string
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, symbol) {
			allDefs = append(allDefs, line)
		}
		if len(allDefs) >= 20 {
			break
		}
	}
	return allDefs, nil
}

func (e *Executor) findDefinitionAST(ctx context.Context, searchRoot, relPrefix, glob, symbol string) ([]string, error) {
	if !treesitter.Enabled() {
		return nil, nil
	}

	var defs []string
	err := filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
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

		rel, err := filepath.Rel(searchRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
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

		defList, err := treesitter.ListDefinitions(path, content)
		if err != nil {
			if errors.Is(err, treesitter.ErrDisabled) || errors.Is(err, treesitter.ErrUnsupported) {
				return nil
			}
			return nil
		}
		for _, def := range defList {
			if def.Name == symbol {
				defs = append(defs, fmt.Sprintf("%s:%d (%s)", rel, def.Line, def.Kind))
				if len(defs) >= 20 {
					return nil
				}
			}
		}
		return nil
	})
	return defs, err
}
