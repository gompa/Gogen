//go:build !cgo

package treesitter

func listDefinitions(path string, content []byte) ([]Definition, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	return nil, ErrUnsupported
}

func findSymbolReferences(path string, content []byte, symbol string) ([]Reference, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	return nil, ErrUnsupported
}

func refsQueryText(langName string) string {
	return ""
}

// ReferenceSearchSupported reports whether AST reference search is available for path.
func ReferenceSearchSupported(path string) bool {
	return false
}

func checkSupported(path string, content []byte) []Issue {
	return nil
}

func BundledLanguages() []string {
	return nil
}
