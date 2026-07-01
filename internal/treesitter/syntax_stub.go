//go:build !cgo

package treesitter

func checkSupported(path string, content []byte) []Issue {
	return nil
}

func BundledLanguages() []string {
	return nil
}
