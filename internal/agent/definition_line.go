package agent

import (
	"regexp"
	"sort"
	"strings"
)

// definitionKeywords maps each recognized definition keyword to its
// normalized kind. This table is the single source of truth for line-based
// "is this line a definition?" detection: the text outline
// (list_definitions fallback), isDefinitionLine (call graph), and the
// ripgrep patterns used by the find_symbol and call_graph text fallbacks
// all derive from it, so the tools cannot drift apart.
var definitionKeywords = map[string]string{
	"func":      "func",
	"fn":        "func",
	"function":  "func",
	"def":       "func",
	"class":     "class",
	"struct":    "struct",
	"interface": "interface",
	"enum":      "enum",
	"trait":     "trait",
	"impl":      "impl",
	"type":      "type",
	"const":     "const",
	"var":       "var",
	"let":       "var",
	"mod":       "module",
	"module":    "module",
	"namespace": "namespace",
	"package":   "package",
}

// definitionModifiers are visibility/scope qualifiers that may precede a
// definition keyword on the same line.
var definitionModifiers = []string{
	"public", "private", "protected", "internal", "export", "pub",
	"static", "final", "abstract", "virtual", "override", "async",
	"extern", "unsafe",
}

// definitionLinePattern matches a line that plausibly opens a declaration:
// optional visibility/scope modifiers, then a definition keyword, then the
// rest of the line. It is built from definitionKeywords so the keyword set
// has a single source of truth. Line-based and approximate — the tree-sitter
// AST path is the primary implementation; this only backs the text fallbacks.
var definitionLinePattern = regexp.MustCompile(buildDefinitionLinePattern())

// buildDefinitionLinePattern renders the declaration-line regex from the
// shared keyword and modifier tables. Keywords are sorted for a deterministic
// pattern; the trailing \b makes alternation order irrelevant (e.g. `func`
// never matches inside `function`).
func buildDefinitionLinePattern() string {
	keys := make([]string, 0, len(definitionKeywords))
	for k := range definitionKeywords {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return `(?m)^[ \t]*(?:(?:` + strings.Join(definitionModifiers, "|") + `)\s+)*(` +
		strings.Join(keys, "|") + `)\b([^\n]*)`
}

// classifyDefinitionLine inspects a single source line and reports whether it
// opens a named declaration. On success it returns the normalized kind, the
// declared name, and the trimmed remainder of the line after the name. The
// name is the first identifier after the keyword, skipping a parenthesized
// Go receiver (depth-aware, so function-typed receivers work) and a generic
// parameter list, so `func (s *S) M()`, `func (f func()) M()`,
// `func M[T any]()` and `type Foo[T any]` all yield their declared name.
// ok is false when the line is not a declaration or no name can be extracted
// (e.g. a grouped `type (` line).
func classifyDefinitionLine(line string) (kind, name, restAfterName string, ok bool) {
	m := definitionLinePattern.FindStringSubmatch(line)
	if m == nil {
		return "", "", "", false
	}
	kind = definitionKeywords[m[1]]
	rest := strings.TrimSpace(m[2])
	rest = skipGoReceiver(rest)
	// Strip generics: `Foo[T any]` names Foo.
	if i := strings.IndexByte(rest, '['); i > 0 {
		rest = rest[:i]
	}
	// Take the leading identifier token; remember what follows the name.
	name = rest
	for i, r := range rest {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			name = strings.TrimSpace(rest[:i])
			restAfterName = strings.TrimSpace(rest[i:])
			break
		}
	}
	if name == "" {
		return "", "", "", false
	}
	return kind, name, restAfterName, true
}

// skipGoReceiver strips a leading Go receiver (e.g. "(s *Server)") from a
// definition line so the method name follows. Parentheses are matched by
// depth, so function-typed receivers like "(f func())" are handled.
// Non-receiver input is returned unchanged.
func skipGoReceiver(rest string) string {
	if !strings.HasPrefix(rest, "(") {
		return rest
	}
	depth := 0
	end := -1
	for i, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end >= 0 {
		return strings.TrimSpace(rest[end+1:])
	}
	return rest
}

// definitionSearchPattern builds a regular expression matching lines that
// plausibly define symbol. kinds restricts the normalized kinds included
// (empty = all kinds); func-kind keywords get both the plain form
// (`func M(`) and, for Go, the receiver form (`func (s *S) M(`). The pattern
// is a cheap recall filter for ripgrep — callers gate the matches with
// classifyDefinitionLine/isDefinitionLine for precision, so the keyword
// coverage stays in sync across tools.
func definitionSearchPattern(symbol string, kinds ...string) string {
	q := regexp.QuoteMeta(symbol)
	var want map[string]bool
	if len(kinds) > 0 {
		want = make(map[string]bool, len(kinds))
		for _, k := range kinds {
			want[k] = true
		}
	}
	keys := make([]string, 0, len(definitionKeywords))
	for k := range definitionKeywords {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	branches := make([]string, 0, len(keys)*2)
	for _, kw := range keys {
		kind := definitionKeywords[kw]
		if want != nil && !want[kind] {
			continue
		}
		if kind == "func" {
			branches = append(branches, `\b`+kw+`\s+`+q+`\s*\(`)
			if kw == "func" {
				branches = append(branches, `\bfunc\s*\([^)]*\)\s+`+q+`\s*\(`)
			}
		} else {
			branches = append(branches, `\b`+kw+`\s+`+q+`\b`)
		}
	}
	return `(?m)(?:` + strings.Join(branches, "|") + `)`
}
