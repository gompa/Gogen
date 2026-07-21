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

// isDefinitionLine checks whether a line is a definition of the symbol in any language.
func isDefinitionLine(line, symbol string) bool {
	trimmed := strings.TrimSpace(line)
	// Go: func Symbol( or func (receiver) Symbol(
	if strings.HasPrefix(trimmed, "func ") {
		rest := trimmed[5:]
		// Skip receiver: func (x *T) Symbol(
		if strings.HasPrefix(rest, "(") {
			if idx := strings.Index(rest, ")"); idx >= 0 {
				rest = strings.TrimSpace(rest[idx+1:])
			}
		}
		return strings.HasPrefix(rest, symbol+"(") || strings.HasPrefix(rest, symbol+" ")
	}
	// Python: def Symbol(
	if strings.HasPrefix(trimmed, "def "+symbol+"(") {
		return true
	}
	// JavaScript/TypeScript: function Symbol( or const Symbol = (
	if strings.HasPrefix(trimmed, "function "+symbol+"(") {
		return true
	}
	if strings.HasPrefix(trimmed, "const "+symbol) || strings.HasPrefix(trimmed, "let "+symbol) || strings.HasPrefix(trimmed, "var "+symbol) {
		after := trimmed[len(symbol)+6:]
		after = strings.TrimSpace(after)
		if strings.HasPrefix(after, "= (") || strings.HasPrefix(after, "=(") {
			return true
		}
	}
	return false
}

// isCallSite checks whether a line contains a call to the symbol (not a definition).
func isCallSite(line, symbol string) bool {
	if isDefinitionLine(line, symbol) {
		return false
	}
	// Look for symbol( or .symbol(
	idx := strings.Index(line, symbol+"(")
	if idx < 0 {
		idx = strings.Index(line, "."+symbol+"(")
		if idx >= 0 {
			idx++ // skip the dot
		}
	}
	return idx >= 0
}

func (e *Executor) callGraphWithAST(ctx context.Context, searchRoot, relPrefix, glob, symbol string) (*CallGraphResult, error) {
	result := &CallGraphResult{Symbol: symbol}

	err := e.walkSymbolReferences(ctx, searchRoot, relPrefix, glob, symbol,
		func(filePath string, refs []treesitter.Reference, content []byte) error {
			lines := strings.Split(string(content), "\n")

			// Find the function definition for this symbol to extract callees.
			defLine := findDefinitionLine(lines, symbol)

			for _, ref := range refs {
				if ref.Line-1 >= len(lines) {
					continue
				}
				line := lines[ref.Line-1]

				// Call sites are the callers of the symbol.
				if isCallSite(line, symbol) {
					result.Callers = append(result.Callers, FunctionRef{
						Name: symbol,
						File: filePath,
						Line: ref.Line,
					})
				}
			}

			// Extract callees from the function body.
			if defLine > 0 {
				callees := extractCalleesFromBody(lines, defLine, symbol)
				for _, c := range callees {
					result.Callees = append(result.Callees, FunctionRef{
						Name: c,
						File: filePath,
						Line: defLine, // approximate: function definition line
					})
				}
			}

			return nil
		})

	// Deduplicate
	result.Callers = dedupeFunctionRefs(result.Callers)
	result.Callees = dedupeFunctionRefs(result.Callees)
	return result, err
}

func (e *Executor) callGraphWithText(ctx context.Context, searchRoot, relPrefix, glob, symbol string) (*CallGraphResult, error) {
	result := &CallGraphResult{Symbol: symbol}

	// Search for call sites of the symbol → these are the Callers.
	callPattern := `\b` + regexp.QuoteMeta(symbol) + `\s*\(`
	err := e.walkSymbolReferencesText(ctx, searchRoot, relPrefix, glob, callPattern,
		func(filePath string, lineNum int, line string) error {
			if !isCallSite(line, symbol) {
				return nil
			}
			result.Callers = append(result.Callers, FunctionRef{
				Name: symbol,
				File: filePath,
				Line: lineNum,
			})
			return nil
		})
	if err != nil {
		return nil, err
	}

	// Search for the function definition and extract callees from its body.
	defPattern := `(?:func|def|function)\s+` + regexp.QuoteMeta(symbol) + `\s*\(`
	_ = e.walkSymbolReferencesText(ctx, searchRoot, relPrefix, glob, defPattern,
		func(filePath string, lineNum int, line string) error {
			// Found the definition at lineNum. Extract callees from the definition line
			// and surrounding context. We use a best-effort approach here since we
			// don't have the full file content in this callback.
			callees := extractCalleesFromLine(line, symbol)
			for _, c := range callees {
				result.Callees = append(result.Callees, FunctionRef{
					Name: c,
					File: filePath,
					Line: lineNum,
				})
			}
			return nil
		})

	// Deduplicate results
	result.Callers = dedupeFunctionRefs(result.Callers)
	result.Callees = dedupeFunctionRefs(result.Callees)

	return result, nil
}

// findDefinitionLine returns the 1-based line number of the symbol's definition, or 0 if not found.
func findDefinitionLine(lines []string, symbol string) int {
	for i, line := range lines {
		if isDefinitionLine(line, symbol) {
			return i + 1
		}
	}
	return 0
}

// extractCalleesFromBody finds function calls within the function body starting at defLine.
// It uses brace counting to find the function body, then extracts identifiers followed by `(`.
func extractCalleesFromBody(lines []string, defLine int, symbol string) []string {
	if defLine <= 0 || defLine > len(lines) {
		return nil
	}

	// Find the opening brace of the function body.
	start := defLine - 1 // 0-based
	braceDepth := 0
	bodyStart := -1
	for i := start; i < len(lines); i++ {
		for _, ch := range lines[i] {
			if ch == '{' {
				braceDepth++
				if bodyStart < 0 {
					bodyStart = i + 1 // 1-based line of opening brace
				}
			}
			if ch == '}' {
				braceDepth--
			}
		}
		if bodyStart > 0 && braceDepth == 0 {
			// Found the closing brace.
			return extractCallsFromLines(lines, bodyStart, i, symbol)
		}
	}
	// If we didn't find a closing brace, scan to end of file.
	if bodyStart > 0 {
		return extractCallsFromLines(lines, bodyStart, len(lines), symbol)
	}
	return nil
}

// extractCallsFromLines finds function calls (identifiers followed by `(`) in the given line range,
// excluding the symbol itself.
func extractCallsFromLines(lines []string, start, end int, symbol string) []string {
	seen := make(map[string]bool)
	var callees []string

	callRe := regexp.MustCompile(`\b([a-zA-Z_]\w*)\s*\(`)
	for i := start - 1; i < end && i < len(lines); i++ {
		line := lines[i]
		matches := callRe.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			name := m[1]
			// Skip the symbol itself and common keywords.
			if name == symbol || isBuiltinOrKeyword(name) {
				continue
			}
			if !seen[name] {
				seen[name] = true
				callees = append(callees, name)
			}
		}
	}
	return callees
}

// extractCalleesFromLine extracts function calls from a single line (best-effort for text search).
func extractCalleesFromLine(line, symbol string) []string {
	seen := make(map[string]bool)
	var callees []string

	callRe := regexp.MustCompile(`\b([a-zA-Z_]\w*)\s*\(`)
	matches := callRe.FindAllStringSubmatch(line, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if name == symbol || isBuiltinOrKeyword(name) {
			continue
		}
		if !seen[name] {
			seen[name] = true
			callees = append(callees, name)
		}
	}
	return callees
}

// isBuiltinOrKeyword checks if a name is a common keyword, builtin, or package import.
func isBuiltinOrKeyword(name string) bool {
	keywords := map[string]bool{
		// Go keywords and builtins
		"true": true, "false": true, "nil": true,
		"len": true, "cap": true, "make": true, "new": true, "append": true,
		"delete": true, "copy": true, "close": true, "panic": true, "recover": true,
		"print": true, "println": true,
		// Control flow keywords (all languages)
		"if": true, "else": true, "for": true, "while": true, "switch": true,
		"case": true, "default": true, "return": true, "range": true,
		"break": true, "continue": true, "fallthrough": true,
		// Common package names that appear as function call targets
		"fmt": true, "log": true, "os": true, "io": true, "strings": true,
		"strconv": true, "filepath": true, "path": true, "context": true,
		"time": true, "json": true, "http": true, "exec": true, "regexp": true,
		"sort": true, "math": true, "sync": true, "errors": true,
		// JS/Python keywords
		"null": true, "undefined": true, "self": true, "cls": true,
		"import": true, "from": true, "class": true, "try": true, "catch": true,
		"finally": true, "raise": true, "yield": true, "async": true, "await": true,
	}
	return keywords[name]
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
			fmt.Fprintf(&b, "  at %s:%d\n", c.File, c.Line)
		}
		b.WriteString("\n")
	}

	if direction != "callers" && len(result.Callees) > 0 {
		b.WriteString("Callees (what this calls):\n")
		for _, c := range result.Callees {
			fmt.Fprintf(&b, "  %s() at %s:%d\n", c.Name, c.File, c.Line)
		}
	}

	if direction != "callees" && len(result.Callers) == 0 {
		b.WriteString("Callers: none found\n\n")
	}
	if direction != "callers" && len(result.Callees) == 0 {
		b.WriteString("Callees: none found (or could not be determined)\n")
	}

	return b.String()
}
