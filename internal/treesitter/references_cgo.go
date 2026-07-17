//go:build cgo

package treesitter

import (
	"fmt"
	"sync"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

var (
	refsQueryOnce  sync.Once
	refsQueryCache sync.Map // lang -> *tree_sitter.Query
)

func refsQueryText(langName string) string {
	q, ok := refsQueries[langName]
	if !ok {
		return ""
	}
	return q
}

var refsQueries = map[string]string{
	"go": `(identifier) @ref
(field_identifier) @ref
(type_identifier) @ref`,
	"python": `(identifier) @ref`,
	"javascript": `(identifier) @ref
(property_identifier) @ref
(shorthand_property_identifier) @ref`,
	"typescript": `(identifier) @ref
(property_identifier) @ref
(shorthand_property_identifier) @ref
(type_identifier) @ref`,
	"tsx": `(identifier) @ref
(property_identifier) @ref
(shorthand_property_identifier) @ref
(type_identifier) @ref`,
	"rust": `(identifier) @ref
(field_identifier) @ref
(type_identifier) @ref`,
	"java": `(identifier) @ref`,
	"c": `(identifier) @ref
(field_identifier) @ref
(type_identifier) @ref`,
	"cpp": `(identifier) @ref
(field_identifier) @ref
(type_identifier) @ref`,
	"csharp": `(identifier) @ref`,
	"php": `(name) @ref
(variable_name) @ref`,
	"ruby": `(identifier) @ref
(constant) @ref`,
	"bash": `(word) @ref`,
	"lua": `(identifier) @ref`,
	"hcl": `(identifier) @ref`,
}

func refsQueryForLang(langName string) (*tree_sitter.Query, error) {
	refsQueryOnce.Do(func() {})
	if v, ok := refsQueryCache.Load(langName); ok {
		return v.(*tree_sitter.Query), nil
	}
	src := refsQueryText(langName)
	if src == "" {
		return nil, ErrUnsupported
	}
	lang := languageFor(langName)
	if lang == nil {
		return nil, ErrUnsupported
	}
	q, err := tree_sitter.NewQuery(lang, src)
	if err != nil {
		return nil, fmt.Errorf("compile refs query for %s: %w", langName, err)
	}
	refsQueryCache.Store(langName, q)
	return q, nil
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
	parser.SetLanguage(lang)

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
				Line: line,
				Text: lineTextAt(content, line),
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
	return refsQueryText(lang) != ""
}
