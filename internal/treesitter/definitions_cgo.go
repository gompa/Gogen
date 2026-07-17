//go:build cgo

package treesitter

import (
	"embed"
	"fmt"
	"strings"
	"sync"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

//go:embed queries/*.scm
var queryFS embed.FS

var (
	queryOnce  sync.Once
	queryByLang map[string]string
	queryCache sync.Map // lang -> *tree_sitter.Query
)

func initQueries() {
	queryByLang = map[string]string{
		"go":         "queries/go.scm",
		"python":     "queries/python.scm",
		"javascript": "queries/javascript.scm",
		"typescript": "queries/typescript.scm",
		"tsx":        "queries/tsx.scm",
		"rust":       "queries/rust.scm",
		"java":       "queries/java.scm",
		"c":          "queries/c.scm",
		"cpp":        "queries/cpp.scm",
		"csharp":     "queries/csharp.scm",
		"php":        "queries/php.scm",
		"ruby":       "queries/ruby.scm",
		"bash":       "queries/bash.scm",
		"lua":        "queries/lua.scm",
		"hcl":        "queries/hcl.scm",
	}
}

func queryForLang(langName string) (*tree_sitter.Query, error) {
	queryOnce.Do(initQueries)
	if v, ok := queryCache.Load(langName); ok {
		return v.(*tree_sitter.Query), nil
	}
	path, ok := queryByLang[langName]
	if !ok {
		return nil, ErrUnsupported
	}
	src, err := queryFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read query %s: %w", path, err)
	}
	lang := languageFor(langName)
	if lang == nil {
		return nil, ErrUnsupported
	}
	q, qerr := tree_sitter.NewQuery(lang, string(src))
	if qerr != nil {
		return nil, fmt.Errorf("compile query for %s: %w", langName, qerr)
	}
	queryCache.Store(langName, q)
	return q, nil
}

const maxDefinitions = 300

// parserPool reuses tree-sitter parsers to avoid C FFI allocation overhead
// on every parse call. Parsers are safe to reuse after SetLanguage.
var parserPool = sync.Pool{
	New: func() interface{} {
		return tree_sitter.NewParser()
	},
}

func listDefinitions(path string, content []byte) ([]Definition, error) {
	langName, ok := langNameForPath(path)
	if !ok {
		return nil, ErrUnsupported
	}
	query, err := queryForLang(langName)
	if err != nil {
		return nil, err
	}

	lang := languageFor(langName)
	p := parserPool.Get().(*tree_sitter.Parser)
	defer parserPool.Put(p)
	parser := p
	parser.SetLanguage(lang)

	tree := parser.Parse(content, nil)
	if tree == nil {
		return nil, fmt.Errorf("failed to parse %s", path)
	}
	defer tree.Close()

	cursor := tree_sitter.NewQueryCursor()
	defer cursor.Close()

	matches := cursor.Matches(query, tree.RootNode(), content)
	names := query.CaptureNames()

	seen := make(map[string]struct{})
	var defs []Definition
	for {
		match := matches.Next()
		if match == nil {
			break
		}
		for _, cap := range match.Captures {
			if int(cap.Index) >= len(names) {
				continue
			}
			captureName := names[cap.Index]
			if !strings.HasPrefix(captureName, "name.") {
				continue
			}
			kind := strings.TrimPrefix(captureName, "name.")
			name := strings.TrimSpace(cap.Node.Utf8Text(content))
			if name == "" {
				continue
			}
			line := int(cap.Node.StartPosition().Row) + 1
			key := fmt.Sprintf("%d:%s:%s", line, kind, name)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			defs = append(defs, Definition{Line: line, Kind: kind, Name: name})
			if len(defs) >= maxDefinitions {
				return defs, nil
			}
		}
	}
	return defs, nil
}
