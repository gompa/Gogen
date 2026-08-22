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

// queryForLang returns the compiled definition query for langName, sourced
// from the registry's defsQuery embed path and compiled through the shared
// query cache (compileQuery).
func queryForLang(langName string) (*tree_sitter.Query, error) {
	registryOnce.Do(initRegistry)
	spec, ok := langSpecs[langName]
	if !ok || spec.defsQuery == "" {
		return nil, ErrUnsupported
	}
	src, err := queryFS.ReadFile(spec.defsQuery)
	if err != nil {
		return nil, fmt.Errorf("read query %s: %w", spec.defsQuery, err)
	}
	return compileQuery(langName, "defs", string(src))
}

const maxDefinitions = 300

// parserPool reuses tree-sitter parsers to avoid C FFI allocation overhead
// on every parse call. Parsers are safe to reuse after SetLanguage.
var parserPool = sync.Pool{
	New: func() any {
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
	if err := parser.SetLanguage(lang); err != nil {
		return nil, fmt.Errorf("set language %s: %w", langName, err)
	}

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
			if langName == "ruby" {
				name = strings.TrimPrefix(name, ":")
			}
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
