package projectfile

import "testing"

func TestMergeBoolAcceptsOnOffVariants(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{"true", "true", true},
		{"1", "1", true},
		{"on", "on", true},
		{"ON", "ON", true},
		{"yes", "yes", true},
		{"false", "false", false},
		{"0", "0", false},
		{"off", "off", false},
		{"no", "no", false},
		{"invalid falls back to default", "maybe", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOGEN_TEST_BOOL", tt.env)
			got := mergeBool("GOGEN_TEST_BOOL", FileConfig{}, "", false, false)
			if got != tt.want {
				t.Errorf("mergeBool(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestMergeIntAndFloatFallBackToDefaultOnInvalidEnv(t *testing.T) {
	t.Setenv("GOGEN_TEST_INT", "abc")
	if got := mergeInt("GOGEN_TEST_INT", FileConfig{}, "", 0, 42); got != 42 {
		t.Errorf("mergeInt invalid env = %d, want default 42", got)
	}

	t.Setenv("GOGEN_TEST_FLOAT", "nope")
	if got := mergeFloat("GOGEN_TEST_FLOAT", FileConfig{}, "", 0, 1.5); got != 1.5 {
		t.Errorf("mergeFloat invalid env = %f, want default 1.5", got)
	}
}

func TestMergeBoolEnvOverridesFile(t *testing.T) {
	t.Setenv("GOGEN_TEST_BOOL", "off")
	file := FileConfig{Keys: map[string]struct{}{"cli_verbose": {}}, CLIVerbose: true}
	if got := mergeBool("GOGEN_TEST_BOOL", file, "cli_verbose", file.CLIVerbose, false); got {
		t.Error("env GOGEN_TEST_BOOL=off should override file cli_verbose: true")
	}
}

func TestMergeBoolFileValueUsedWhenEnvUnset(t *testing.T) {
	t.Setenv("GOGEN_TEST_BOOL_UNUSED", "") // ensure a distinct var is unset
	file := FileConfig{Keys: map[string]struct{}{"cli_verbose": {}}, CLIVerbose: true}
	if got := mergeBool("GOGEN_TEST_BOOL", file, "cli_verbose", file.CLIVerbose, false); !got {
		t.Error("file cli_verbose: true should win when env is unset")
	}
}
