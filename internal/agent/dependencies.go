package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gogen/internal/treesitter"
)

type DependencyResult struct {
	Symbol             string
	DirectDependents   []string
	IndirectDependents []string
	ImpactScore        int
	Recommendation     string
	Method             string
}

// DependencyAnalysis analyzes the impact of changing a symbol.
// Uses tree-sitter for supported languages, falls back to text search.
func (e *Executor) DependencyAnalysis(ctx context.Context, symbol, subpath string) (string, error) {
	if symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	// Use AST fallback pattern: try AST first, then text search
	fallback := &ASTFallback[*DependencyResult]{
		ASTFunc: func() (*DependencyResult, error) {
			return e.dependencyAnalysisWithAST(ctx, searchRoot, relPrefix, symbol)
		},
		TextFunc: func() (*DependencyResult, error) {
			return e.dependencyAnalysisWithText(ctx, searchRoot, relPrefix, symbol)
		},
		HasResult: func(r *DependencyResult) bool {
			return r != nil && len(r.DirectDependents) > 0
		},
	}

	result, err := fallback.Run()
	if err != nil {
		return "", err
	}
	return formatDependencyResult(result), nil
}

func (e *Executor) dependencyAnalysisWithAST(ctx context.Context, searchRoot, relPrefix, symbol string) (*DependencyResult, error) {
	result := &DependencyResult{Symbol: symbol}

	err := e.walkSymbolReferences(ctx, searchRoot, relPrefix, "", symbol,
		func(filePath string, refs []treesitter.Reference, content []byte) error {
			result.DirectDependents = append(result.DirectDependents, filePath)
			return nil
		})

	if err != nil {
		return nil, err
	}

	// Find indirect dependents
	result.IndirectDependents = e.findIndirectDependents(ctx, result.DirectDependents, "")

	// Calculate impact score
	result.ImpactScore = len(result.DirectDependents) + len(result.IndirectDependents)*2

	// Generate recommendation
	if result.ImpactScore > 20 {
		result.Recommendation = "⚠️  High impact change - consider breaking into smaller changes"
	} else if result.ImpactScore > 10 {
		result.Recommendation = "⚡ Medium impact change"
	} else {
		result.Recommendation = "✅ Low impact change"
	}

	return result, nil
}

func (e *Executor) dependencyAnalysisWithText(ctx context.Context, searchRoot, relPrefix, symbol string) (*DependencyResult, error) {
	result := &DependencyResult{Symbol: symbol}

	// Use shared text-based search helper
	pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
	seenFiles := make(map[string]bool)
	err := e.walkSymbolReferencesText(ctx, searchRoot, relPrefix, "", pattern,
		func(filePath string, lineNum int, line string) error {
			if !seenFiles[filePath] {
				seenFiles[filePath] = true
				result.DirectDependents = append(result.DirectDependents, filePath)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	// Find indirect dependents
	result.IndirectDependents = e.findIndirectDependents(ctx, result.DirectDependents, "")

	// Calculate impact score
	result.ImpactScore = len(result.DirectDependents) + len(result.IndirectDependents)*2

	// Generate recommendation
	if result.ImpactScore > 20 {
		result.Recommendation = "⚠️  High impact change - consider breaking into smaller changes"
	} else if result.ImpactScore > 10 {
		result.Recommendation = "⚡ Medium impact change"
	} else {
		result.Recommendation = "✅ Low impact change"
	}

	return result, nil
}

func (e *Executor) findIndirectDependents(ctx context.Context, directDependents []string, subpath string) []string {
	indirect := make(map[string]bool)

	for _, dep := range directDependents {
		// Extract filename without extension
		fileName := filepath.Base(dep)
		nameWithoutExt := strings.TrimSuffix(fileName, filepath.Ext(fileName))

		// Search for references to this file
		pattern := regexp.QuoteMeta(nameWithoutExt)
		results, err := e.SearchCode(ctx, pattern, subpath, "", 0)
		if err != nil {
			continue
		}

		// Parse results
		lines := strings.Split(results, "\n")
		for _, line := range lines {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) >= 1 && parts[0] != dep {
				indirect[parts[0]] = true
			}
		}
	}

	// Convert to slice
	var result []string
	for file := range indirect {
		result = append(result, file)
	}

	return result
}

func formatDependencyResult(result *DependencyResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Dependency analysis for %q (method: %s):\n\n", result.Symbol, result.Method)

	fmt.Fprintf(&b, "Direct dependents: %d\n", len(result.DirectDependents))
	for _, dep := range result.DirectDependents {
		fmt.Fprintf(&b, "  - %s\n", dep)
	}

	fmt.Fprintf(&b, "\nIndirect dependents: %d\n", len(result.IndirectDependents))
	for _, dep := range result.IndirectDependents {
		fmt.Fprintf(&b, "  - %s\n", dep)
	}

	fmt.Fprintf(&b, "\nImpact score: %d\n", result.ImpactScore)
	fmt.Fprintf(&b, "%s\n", result.Recommendation)

	return b.String()
}
