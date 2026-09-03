package agent

import (
	"context"
	"fmt"
	"regexp"
	"slices"
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

// computeImpact sets ImpactScore and Recommendation based on direct and indirect dependents.
func (r *DependencyResult) computeImpact() {
	if r == nil {
		return
	}
	r.ImpactScore = len(r.DirectDependents) + len(r.IndirectDependents)*2
	switch {
	case r.ImpactScore > 20:
		r.Recommendation = "⚠️  High impact change - consider breaking into smaller changes"
	case r.ImpactScore > 10:
		r.Recommendation = "⚡ Medium impact change"
	default:
		r.Recommendation = "✅ Low impact change"
	}
}

// DependencyAnalysis analyzes the impact of changing a symbol.
// Uses tree-sitter for supported languages, falls back to text search.
// It powers the call_graph tool's "impact" direction: dependents (all
// references, not just call sites), transitive blast radius, and a risk
// score/recommendation.
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
	result := &DependencyResult{Symbol: symbol, Method: "ast"}

	err := e.walkSymbolReferences(ctx, searchRoot, relPrefix, "", symbol,
		func(filePath string, refs []treesitter.Reference, content []byte) error {
			result.DirectDependents = append(result.DirectDependents, filePath)
			return nil
		})

	if err != nil {
		return nil, err
	}

	// Find indirect dependents
	result.IndirectDependents = e.findIndirectDependents(ctx, result.DirectDependents)

	result.computeImpact()

	return result, nil
}

func (e *Executor) dependencyAnalysisWithText(ctx context.Context, searchRoot, relPrefix, symbol string) (*DependencyResult, error) {
	result := &DependencyResult{Symbol: symbol, Method: "text"}

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
	result.IndirectDependents = e.findIndirectDependents(ctx, result.DirectDependents)

	result.computeImpact()

	return result, nil
}

func (e *Executor) findIndirectDependents(ctx context.Context, directDependents []string) []string {
	indirect := make(map[string]bool)

	for _, dep := range directDependents {
		// Search for references to the relative path (e.g. "internal/agent/utils").
		// This is more precise than searching for just the filename stem.
		pattern := regexp.QuoteMeta(dep)
		results, err := e.SearchCode(ctx, pattern, "", "", 0, false)
		if err != nil {
			continue
		}
		if strings.HasPrefix(results, "No matches") {
			continue
		}

		// Parse results
		lines := strings.Split(results, "\n")
		for _, line := range lines {
			file, _, ok := splitSearchLine(line)
			if ok && file != dep {
				indirect[file] = true
			}
		}
	}

	// Convert to a sorted slice: map iteration order is randomized, and a
	// deterministic order keeps repeated impact analyses byte-identical
	// (prompt-cache stable across retries).
	var result []string
	for file := range indirect {
		result = append(result, file)
	}
	slices.Sort(result)

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
