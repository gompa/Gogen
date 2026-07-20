package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gogen/internal/treesitter"
)

type CallGraphResult struct {
	Symbol  string
	Callers []FunctionRef
	Callees []FunctionRef
	Method  string // "ast" or "text"
}

type FunctionRef struct {
	Name string
	File string
	Line int
}

// CallGraph analyzes the call graph for a symbol.
// Uses tree-sitter for supported languages, falls back to text patterns.
func (e *Executor) CallGraph(ctx context.Context, symbol, subpath, glob string, direction string) (string, error) {
	if symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	// Use AST fallback pattern: try AST first, then text search
	fallback := &ASTFallback[*CallGraphResult]{
		ASTFunc: func() (*CallGraphResult, error) {
			return e.callGraphWithAST(ctx, searchRoot, relPrefix, glob, symbol)
		},
		TextFunc: func() (*CallGraphResult, error) {
			return e.callGraphWithText(ctx, searchRoot, relPrefix, glob, symbol)
		},
		HasResult: func(r *CallGraphResult) bool {
			return r != nil && (len(r.Callers) > 0 || len(r.Callees) > 0)
		},
	}

	result, err := fallback.Run()
	if err != nil {
		return "", err
	}
	return formatCallGraph(result, direction), nil
}

func (e *Executor) callGraphWithAST(ctx context.Context, searchRoot, relPrefix, glob, symbol string) (*CallGraphResult, error) {
	result := &CallGraphResult{Symbol: symbol}

	err := e.walkSymbolReferences(ctx, searchRoot, relPrefix, glob, symbol,
		func(filePath string, refs []treesitter.Reference, content []byte) error {
			// Analyze each reference to determine if it's a call or definition
			lines := strings.Split(string(content), "\n")
			for _, ref := range refs {
				if ref.Line-1 < len(lines) {
					line := lines[ref.Line-1]

					// Check if this is a function call
					if strings.Contains(line, symbol+"(") || strings.Contains(line, "."+symbol+"(") {
						result.Callees = append(result.Callees, FunctionRef{
							Name: symbol,
							File: filePath,
							Line: ref.Line,
						})
					}

					// Check if this is a function definition
					if strings.Contains(line, "func "+symbol) || strings.Contains(line, "def "+symbol) ||
						strings.Contains(line, "function "+symbol) {
						result.Callers = append(result.Callers, FunctionRef{
							Name: symbol,
							File: filePath,
							Line: ref.Line,
						})
					}
				}
			}
			return nil
		})

	return result, err
}

func (e *Executor) callGraphWithText(ctx context.Context, searchRoot, relPrefix, glob, symbol string) (*CallGraphResult, error) {
	result := &CallGraphResult{Symbol: symbol}

	// Use shared text-based search helper
	callPattern := `\b` + regexp.QuoteMeta(symbol) + `\s*\(`
	err := e.walkSymbolReferencesText(ctx, searchRoot, relPrefix, glob, callPattern,
		func(filePath string, lineNum int, line string) error {
			result.Callees = append(result.Callees, FunctionRef{
				Name: symbol,
				File: filePath,
				Line: lineNum,
			})
			return nil
		})
	if err != nil {
		return nil, err
	}

	// Deduplicate results
	result.Callers = dedupeFunctionRefs(result.Callers)
	result.Callees = dedupeFunctionRefs(result.Callees)

	return result, nil
}

func dedupeFunctionRefs(refs []FunctionRef) []FunctionRef {
	seen := make(map[string]bool)
	var result []FunctionRef

	for _, ref := range refs {
		key := fmt.Sprintf("%s:%s:%d", ref.Name, ref.File, ref.Line)
		if !seen[key] {
			seen[key] = true
			result = append(result, ref)
		}
	}

	return result
}

func formatCallGraph(result *CallGraphResult, direction string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Call graph for %q (method: %s):\n\n", result.Symbol, result.Method)

	if direction != "callees" && len(result.Callers) > 0 {
		b.WriteString("Callers (who calls this):\n")
		for _, c := range result.Callers {
			fmt.Fprintf(&b, "  %s() at %s:%d\n", c.Name, c.File, c.Line)
		}
		b.WriteString("\n")
	}

	if direction != "callers" && len(result.Callees) > 0 {
		b.WriteString("Callees (what this calls):\n")
		for _, c := range result.Callees {
			fmt.Fprintf(&b, "  %s() at %s:%d\n", c.Name, c.File, c.Line)
		}
	}

	return b.String()
}
