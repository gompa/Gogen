package agent

import (
	"regexp"
	"testing"
)

// TestClassifyDefinitionLine pins the shared line-based definition
// classifier: plain functions, Go methods with receivers (incl. generic and
// function-typed receivers), generic methods, modifiers, and non-definition
// lines (grouped `type (`, comments, calls) that must be rejected.
func TestClassifyDefinitionLine(t *testing.T) {
	cases := []struct {
		line     string
		wantKind string
		wantName string
		wantRest string
		wantOK   bool
	}{
		// Cases carried over from the old outlineKindName test.
		{"func Hello() {}", "func", "Hello", "() {}", true},
		{"func (s *Server) Handle() {}", "func", "Handle", "() {}", true},
		{"type Widget struct{}", "type", "Widget", "struct{}", true},
		{"type Foo[T any] struct{}", "type", "Foo", "", true},
		{"def greet(name):", "func", "greet", "(name):", true},
		{"type (", "", "", "", false},
		// Receivers and generics.
		{"func (s *S[T]) M()", "func", "M", "()", true},
		{"func (f func()) M()", "func", "M", "()", true},
		{"func M[T any]()", "func", "M", "", true},
		// Languages and modifiers.
		{"    def spaced(x):", "func", "spaced", "(x):", true},
		{"public class Widget {", "class", "Widget", "{", true},
		{"export async function load() {}", "func", "load", "() {}", true},
		{"fn parse(input: &str) -> bool {", "func", "parse", "(input: &str) -> bool {", true},
		{"const answer = 42", "const", "answer", "= 42", true},
		{"let M = (x) => x * 2", "var", "M", "= (x) => x * 2", true},
		{"var count int", "var", "count", "int", true},
		{"mod utils;", "module", "utils", ";", true},
		{"interface Reader {", "interface", "Reader", "{", true},
		{"package main", "package", "main", "", true},
		// Non-definitions.
		{"x := M()", "", "", "", false},
		{"return func() {}", "", "", "", false},
		{"// func Commented()", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, tc := range cases {
		kind, name, rest, ok := classifyDefinitionLine(tc.line)
		if ok != tc.wantOK || kind != tc.wantKind || name != tc.wantName || rest != tc.wantRest {
			t.Errorf("classifyDefinitionLine(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				tc.line, kind, name, rest, ok, tc.wantKind, tc.wantName, tc.wantRest, tc.wantOK)
		}
	}
}

// TestIsDefinitionLineAgreesWithClassifier pins the call-graph gate: it must
// accept exactly the callable definitions the classifier names (function
// declarations in any supported language, incl. receivers and generics, plus
// JS/TS function-expression bindings) and reject non-callables and calls.
func TestIsDefinitionLineAgreesWithClassifier(t *testing.T) {
	cases := []struct {
		line   string
		symbol string
		want   bool
	}{
		// Go.
		{"func M()", "M", true},
		{"func (s *S) M()", "M", true},
		{"func (s *S[T]) M()", "M", true},
		{"func (f func()) M()", "M", true}, // function-typed receiver
		{"func M[T any]()", "M", true},     // generic method
		{"func (s *S) M()", "s", false},
		// Python / Rust / JS.
		{"def M(x):", "M", true},
		{"fn M(x: i32) -> bool {", "M", true},
		{"function M() {}", "M", true},
		// JS/TS function-expression bindings.
		{"const M = (x) => x", "M", true},
		{"let M = (x) => x", "M", true},
		{"var M = function (x) {}", "M", false}, // named function expressions: known gap, parity with old behavior
		{"const M = 5", "M", false},
		{"const M = M()", "M", false},
		// Non-callable definitions and calls are not definitions here.
		{"class M {", "M", false},
		{"type M struct{}", "M", false},
		{"x := M()", "M", false},
		{"", "M", false},
	}
	for _, tc := range cases {
		if got := isDefinitionLine(tc.line, tc.symbol); got != tc.want {
			t.Errorf("isDefinitionLine(%q, %q) = %v, want %v", tc.line, tc.symbol, got, tc.want)
		}
	}
}

// TestDefinitionSearchPattern pins the generated recall patterns: they must
// match the definition shapes the classifier accepts (including Go methods
// with receivers and JS function declarations) and stay a superset of the
// old hand-written patterns.
func TestDefinitionSearchPattern(t *testing.T) {
	funcRe := regexp.MustCompile(definitionSearchPattern("M", "func"))
	funcCases := []struct {
		line string
		want bool
	}{
		{"func M()", true},
		{"func M(x int) error {", true},
		{"func (s *S) M()", true},
		{"func (s *S[T]) M()", true},
		{"def M():", true},
		{"function M() {}", true},
		{"fn M() {", true},
		{"func M[T any]()", false}, // generic methods: recall gap, gated by isDefinitionLine
		{"func N()", false},
		{"x := M()", false},
	}
	for _, tc := range funcCases {
		if got := funcRe.MatchString(tc.line); got != tc.want {
			t.Errorf("func-kind pattern matched %q = %v, want %v", tc.line, got, tc.want)
		}
	}

	allRe := regexp.MustCompile(definitionSearchPattern("M", "func", "type", "class", "const", "var"))
	allCases := []struct {
		line string
		want bool
	}{
		{"func M()", true},
		{"func (s *S) M()", true},
		{"type M struct{ x int }", true},
		{"type M = Other", true}, // alias: superset of the old suffix-required form
		{"class M extends N {", true},
		{"const M = 1", true},
		{"let M = 1", true},
		{"var M int", true},
		{"interface M {", false}, // kind "interface" not requested
		{"enum M {", false},      // kind "enum" not requested
		{"function M() {}", true},
		{"mod M;", false}, // module kind not requested
		{"x := M()", false},
	}
	for _, tc := range allCases {
		if got := allRe.MatchString(tc.line); got != tc.want {
			t.Errorf("all-kind pattern matched %q = %v, want %v", tc.line, got, tc.want)
		}
	}
}
