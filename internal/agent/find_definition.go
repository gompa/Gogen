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

	// Try tree-sitter first for supported languages.
	astDefs, err := e.findDefinitionAST(ctx, searchRoot, relPrefix, glob, symbol)
	if err != nil {
		return "", err
	}
	if len(astDefs) > 0 {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("Definition(s) for %q (via AST):\n", symbol))
		for _, def := range astDefs {
			b.WriteString(def + "\n")
		}
		b.WriteString(fmt.Sprintf("\n(%d definition(s) found)", len(astDefs)))
		return b.String(), nil
	}

	// Fallback: text search for "func <symbol>", "type <symbol>", "var <symbol>", etc.
	patterns := []string{
		`(?m)\bfunc\s+` + regexp.QuoteMeta(symbol) + `\s*\(`,
		`(?m)\bfunc\s*\([^)]*\)\s+` + regexp.QuoteMeta(symbol) + `\s*\(`,
		`(?m)\btype\s+` + regexp.QuoteMeta(symbol) + `\s+(struct|interface|func|type)`,
		`(?m)\bvar\s+` + regexp.QuoteMeta(symbol) + `\b`,
		`(?m)\bconst\s+` + regexp.QuoteMeta(symbol) + `\b`,
		`(?m)\blet\s+` + regexp.QuoteMeta(symbol) + `\b`,
		`(?m)\bclass\s+` + regexp.QuoteMeta(symbol) + `\b`,
		`(?m)\bdef\s+` + regexp.QuoteMeta(symbol) + `\s*\(`,
	}

	var allDefs []string
	for _, pattern := range patterns {
		out, err := e.SearchCode(ctx, pattern, subpath, glob, 0)
		if err != nil {
			continue
		}
		if strings.HasPrefix(out, "No matches") {
			continue
		}
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if strings.Contains(line, symbol) {
				allDefs = append(allDefs, line)
			}
		}
		if len(allDefs) >= 20 {
			break
		}
	}

	if len(allDefs) == 0 {
		return fmt.Sprintf("No definition found for %q", symbol), nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Definition(s) for %q (text search):\n", symbol))
	for _, def := range allDefs {
		b.WriteString(def + "\n")
	}
	b.WriteString(fmt.Sprintf("\n(%d result(s) — use read_file to inspect)", len(allDefs)))
	return b.String(), nil
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
			if strings.EqualFold(def.Name, symbol) {
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
