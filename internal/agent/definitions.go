package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gogen/internal/treesitter"
)

// definitionLinePattern matches a line that plausibly opens a declaration:
// optional visibility/scope modifiers, then a definition keyword, then the
// rest of the line. Line-based and approximate — the tree-sitter AST path is
// the primary implementation; this only backs the text outline fallback.
var definitionLinePattern = regexp.MustCompile(
	`(?m)^[ \t]*(?:(?:public|private|protected|internal|export|pub|static|final|abstract|virtual|override|async|extern|unsafe)\s+)*(func|fn|function|def|class|struct|interface|enum|trait|impl|type|const|var|let|mod|module|namespace|package)\b([^\n]*)`)

// ListDefinitions returns an outline of named symbols in a source file.
func (e *Executor) ListDefinitions(path string) (string, error) {
	content, err := e.ReadFileRawBytes(path)
	if err != nil {
		return "", err
	}
	defs, err := treesitter.ListDefinitions(path, content)
	if err != nil {
		// Graceful degradation: when tree-sitter is disabled or has no query
		// for this file type, fall back to a line-based text outline instead
		// of erroring, so the tool stays useful in every build.
		if errors.Is(err, treesitter.ErrDisabled) || errors.Is(err, treesitter.ErrUnsupported) {
			return listDefinitionsText(path, content), nil
		}
		return "", err
	}
	return treesitter.FormatDefinitions(path, defs), nil
}

// listDefinitionsMax caps the text-outline size, mirroring the tree-sitter
// path's maxDefinitions cap so huge files cannot flood the conversation.
const listDefinitionsMax = 300

// listDefinitionsText is the text-fallback outline for list_definitions. It
// mirrors the AST output shape (line, kind, name) using a line-based scan of
// common declaration keywords, capped like the AST path.
func listDefinitionsText(path string, content []byte) string {
	type outline struct {
		line int
		kind string
		name string
	}
	var out []outline
	for i, line := range strings.Split(string(content), "\n") {
		m := definitionLinePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		kind, name := outlineKindName(m[1], m[2])
		if name == "" {
			continue
		}
		out = append(out, outline{line: i + 1, kind: kind, name: name})
		if len(out) >= listDefinitionsMax {
			break
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("No definitions found in %s", path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Definitions in %s (%d, text outline):\n", path, len(out))
	for _, d := range out {
		fmt.Fprintf(&b, "L%-4d  %-10s  %s\n", d.line, d.kind, d.name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// outlineKindName maps a matched definition keyword and the remainder of the
// line to a display kind and the declaration's name. The name is the first
// identifier after the keyword, skipping a parenthesized receiver (Go
// methods) or generics so `func (s *S) M()` and `type Foo[T any]` yield M and
// Foo. Returns an empty name when no identifier is present (e.g. a grouped
// `type (` line), which the caller skips.
func outlineKindName(keyword, rest string) (kind, name string) {
	kind = keyword
	switch keyword {
	case "func", "fn", "function", "def":
		kind = "func"
	case "mod":
		kind = "module"
	}
	rest = strings.TrimSpace(rest)
	// Skip a Go receiver before the method name.
	if strings.HasPrefix(rest, "(") {
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
			rest = strings.TrimSpace(rest[end+1:])
		}
	}
	// Strip generics: `Foo[T any]` names Foo.
	if i := strings.IndexByte(rest, '['); i > 0 {
		rest = rest[:i]
	}
	// Take the leading identifier token.
	for i, r := range rest {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			rest = strings.TrimSpace(rest[:i])
			break
		}
	}
	return kind, rest
}
