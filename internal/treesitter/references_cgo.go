//go:build cgo

package treesitter

import (
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
	run, err := runQuery(path, content, refsQueryForLang)
	if err != nil {
		return nil, err
	}
	defer run.Close()

	var refs []Reference
	for cap := range run.Captures() {
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
