package tui

import "testing"

func TestCommandWithArgs(t *testing.T) {
	match := commandWithArgs("contextmode")
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"bare", "contextmode", true},
		{"slash", "/contextmode", true},
		{"arguments", "contextmode deep_dive", true},
		{"slash arguments", "/contextmode deep_dive", true},
		{"prefix is not enough", "contextmodex", false},
		{"other command", "smartcompact on", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := match(tc.input, tc.input); got != tc.want {
				t.Fatalf("commandWithArgs(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
