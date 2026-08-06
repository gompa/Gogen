package projectfile

import (
	"os"
	"testing"
)

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
			got := mergeBool("GOGEN_TEST_BOOL", false, false)
			if got != tt.want {
				t.Errorf("mergeBool(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestMergeIntFallBackToDefaultOnInvalidEnv(t *testing.T) {
	t.Setenv("GOGEN_TEST_INT", "abc")
	if got := mergeInt("GOGEN_TEST_INT", 0, 42); got != 42 {
		t.Errorf("mergeInt invalid env = %d, want default 42", got)
	}
}

func TestMergeBoolEnvOverridesFile(t *testing.T) {
	t.Setenv("GOGEN_TEST_BOOL", "off")
	if got := mergeBool("GOGEN_TEST_BOOL", true, false); got {
		t.Error("env GOGEN_TEST_BOOL=off should override file cli_verbose: true")
	}
}

func TestMergeBoolFileValueUsedWhenEnvUnset(t *testing.T) {
	t.Setenv("GOGEN_TEST_BOOL_UNUSED", "") // ensure a distinct var is unset
	if got := mergeBool("GOGEN_TEST_BOOL", true, false); !got {
		t.Error("file cli_verbose: true should win when env is unset")
	}
}

func TestMergeIntOptPreservesExplicitZero(t *testing.T) {
	os.Unsetenv("GOGEN_TEST_OPT")
	zero := 0
	if got := mergeIntOpt("GOGEN_TEST_OPT", &zero, 12); got != 0 {
		t.Errorf("explicit 0 = %d, want 0", got)
	}
	if got := mergeIntOpt("GOGEN_TEST_OPT", nil, 12); got != 12 {
		t.Errorf("absent = %d, want default 12", got)
	}
	five := 5
	if got := mergeIntOpt("GOGEN_TEST_OPT", &five, 12); got != 5 {
		t.Errorf("explicit 5 = %d, want 5", got)
	}
}

func TestMergeFloatOptPreservesExplicitZero(t *testing.T) {
	os.Unsetenv("GOGEN_TEST_OPTF")
	zero := 0.0
	if got := mergeFloatOpt("GOGEN_TEST_OPTF", &zero, 0.75); got != 0 {
		t.Errorf("explicit 0 = %v, want 0", got)
	}
	if got := mergeFloatOpt("GOGEN_TEST_OPTF", nil, 0.75); got != 0.75 {
		t.Errorf("absent = %v, want default 0.75", got)
	}
}

// End-to-end: explicit zeros in the file must survive the full Merge path so
// the runtime can honor them (auto-compaction off, no tail kept, no cap, no
// reserve), while absent keys fall back to defaults.
func TestMergePreservesExplicitZeros(t *testing.T) {
	for _, env := range []string{"GOGEN_COMPACT_THRESHOLD", "GOGEN_KEEP_RECENT_MESSAGES", "GOGEN_MAX_TOOL_RESULT_BYTES", "GOGEN_COMPACT_RESERVE_TOKENS"} {
		os.Unsetenv(env)
	}
	zero := 0
	zeroF := 0.0
	pf := &ProjectFile{Config: FileConfig{
		CompactThreshold:     &zeroF,
		KeepRecentMessages:   &zero,
		MaxToolResultBytes:   &zero,
		CompactReserveTokens: &zero,
	}}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CompactThreshold != 0 || cfg.KeepRecentMessages != 0 || cfg.MaxToolResultBytes != 0 || cfg.CompactReserveTokens != 0 {
		t.Fatalf("explicit zeros not preserved through Merge: threshold=%v keep=%d max=%d reserve=%d",
			cfg.CompactThreshold, cfg.KeepRecentMessages, cfg.MaxToolResultBytes, cfg.CompactReserveTokens)
	}

	pfAbsent := &ProjectFile{Config: FileConfig{}}
	cfgAbsent := Merge(pfAbsent, FlagOverrides{})
	if cfgAbsent.CompactThreshold != 0.85 || cfgAbsent.KeepRecentMessages != 12 || cfgAbsent.MaxToolResultBytes != 262144 || cfgAbsent.CompactReserveTokens != 4000 {
		t.Fatalf("absent keys should fall back to defaults: threshold=%v keep=%d max=%d reserve=%d",
			cfgAbsent.CompactThreshold, cfgAbsent.KeepRecentMessages, cfgAbsent.MaxToolResultBytes, cfgAbsent.CompactReserveTokens)
	}
}
