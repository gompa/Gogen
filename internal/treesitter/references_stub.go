//go:build !cgo

package treesitter

func refsQueryText(langName string) string {
	return ""
}

func findSymbolReferences(path string, content []byte, symbol string) ([]Reference, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	return nil, ErrUnsupported
}

// ReferenceSearchSupported reports whether AST reference search is available for path.
func ReferenceSearchSupported(path string) bool {
	return false
}
