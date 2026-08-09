package agent

import "testing"

// TestOutlineKindName pins the name extraction used by the text outline:
// plain functions, Go methods with receivers, generics, and empty-name lines
// (grouped `type (`) that callers skip.
func TestOutlineKindName(t *testing.T) {
	cases := []struct {
		keyword, rest, wantKind, wantName string
	}{
		{"func", "Hello() {}", "func", "Hello"},
		{"func", "(s *Server) Handle() {}", "func", "Handle"},
		{"type", "Widget struct{}", "type", "Widget"},
		{"type", "Foo[T any] struct{}", "type", "Foo"},
		{"def", "greet(name):", "func", "greet"},
		{"type", "(", "type", ""},
	}
	for _, tc := range cases {
		kind, name := outlineKindName(tc.keyword, tc.rest)
		if kind != tc.wantKind || name != tc.wantName {
			t.Errorf("outlineKindName(%q, %q) = (%q, %q), want (%q, %q)", tc.keyword, tc.rest, kind, name, tc.wantKind, tc.wantName)
		}
	}
}
