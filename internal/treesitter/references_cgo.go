//go:build cgo

package treesitter

import (
	"fmt"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// refsQueryForLang returns the compiled reference query for langName, sourced
// from the registry's inline refsQuery text and compiled through the shared
// query cache (compileQuery).
func refsQueryForLang(langName string) (*tree_sitter.Query, error) {
	registryOnce.Do(initRegistry)
	spec, ok := langSpecs[langName]
	if !ok || spec.refsQuery == "" {
		return nil, ErrUnsupported
	}
	return compileQuery(langName, "refs", spec.refsQuery)
}

const maxReferencesPerFile = 200

func findSymbolReferences(path string, content []byte, symbol string) ([]Reference, error) {
	langName, ok := langNameForPath(path)
	if !ok {
		return nil, ErrUnsupported
	}
	query, err := refsQueryForLang(langName)
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
	var refs []Reference
	for {
		match := matches.Next()
		if match == nil {
			break
		}
		for _, cap := range match.Captures {
			name := cap.Node.Utf8Text(content)
			if name != symbol {
				continue
			}
			line := int(cap.Node.StartPosition().Row) + 1
			refs = append(refs, Reference{
				Line:  line,
				Start: int(cap.Node.StartByte()),
				End:   int(cap.Node.EndByte()),
				Text:  lineTextAt(content, line),
			})
			if len(refs) >= maxReferencesPerFile {
				sortReferences(refs)
				return dedupeReferences(refs), nil
			}
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	sortReferences(refs)
	return dedupeReferences(refs), nil
}

// ReferenceSearchSupported reports whether AST reference search is available for path.
func ReferenceSearchSupported(path string) bool {
	if !Enabled() {
		return false
	}
	lang, ok := langNameForPath(path)
	if !ok {
		return false
	}
	spec, ok := langSpecs[lang]
	return ok && spec.refsQuery != ""
}
