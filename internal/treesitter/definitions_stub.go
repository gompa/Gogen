//go:build !cgo

package treesitter

func listDefinitions(path string, content []byte) ([]Definition, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	return nil, ErrUnsupported
}
