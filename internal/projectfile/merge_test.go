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

// The pre-rename GOGEN_KEEP_RECENT_MESSAGES env var must keep working, with
// the renamed env var winning when both are set.
func TestMergeLegacyKeepRecentEnv(t *testing.T) {
	t.Setenv("GOGEN_KEEP_RECENT_MESSAGES", "7")
	cfg := Merge(nil, FlagOverrides{})
	if cfg.CompactKeepRecentMessages != 7 {
		t.Fatalf("legacy env not honored: %d, want 7", cfg.CompactKeepRecentMessages)
	}

	t.Setenv("GOGEN_COMPACT_KEEP_RECENT_MESSAGES", "9")
	cfgBoth := Merge(nil, FlagOverrides{})
	if cfgBoth.CompactKeepRecentMessages != 9 {
		t.Fatalf("renamed env should win: %d, want 9", cfgBoth.CompactKeepRecentMessages)
	}
}

// An invalid legacy env value falls back to the default with a warning,
// matching the renamed env's behavior.
func TestMergeLegacyKeepRecentEnvInvalid(t *testing.T) {
	t.Setenv("GOGEN_KEEP_RECENT_MESSAGES", "bogus")
	os.Unsetenv("GOGEN_COMPACT_KEEP_RECENT_MESSAGES")
	cfg := Merge(nil, FlagOverrides{})
	if cfg.CompactKeepRecentMessages != 12 {
		t.Fatalf("invalid legacy env should fall back to default: %d, want 12", cfg.CompactKeepRecentMessages)
	}
}

// The file key aliasing flows through Merge: a project file using the legacy
// key reaches the runtime config even when the new env var is unset.
func TestMergeLegacyKeepRecentFileKey(t *testing.T) {
	os.Unsetenv("GOGEN_COMPACT_KEEP_RECENT_MESSAGES")
	os.Unsetenv("GOGEN_KEEP_RECENT_MESSAGES")
	pf, err := ParseContent("GOGEN.md", "---\nkeep_recent_messages: 20\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CompactKeepRecentMessages != 20 {
		t.Fatalf("legacy file key not honored through Merge: %d, want 20", cfg.CompactKeepRecentMessages)
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
	for _, env := range []string{"GOGEN_COMPACT_THRESHOLD", "GOGEN_COMPACT_KEEP_RECENT_MESSAGES", "GOGEN_MAX_TOOL_RESULT_BYTES", "GOGEN_COMPACT_RESERVE_TOKENS"} {
		os.Unsetenv(env)
	}
	zero := 0
	zeroF := 0.0
	pf := &ProjectFile{Config: FileConfig{
		CompactThreshold:          &zeroF,
		CompactKeepRecentMessages: &zero,
		MaxToolResultBytes:        &zero,
		CompactReserveTokens:      &zero,
	}}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CompactThreshold != 0 || cfg.CompactKeepRecentMessages != 0 || cfg.MaxToolResultBytes != 0 || cfg.CompactReserveTokens != 0 {
		t.Fatalf("explicit zeros not preserved through Merge: threshold=%v keep=%d max=%d reserve=%d",
			cfg.CompactThreshold, cfg.CompactKeepRecentMessages, cfg.MaxToolResultBytes, cfg.CompactReserveTokens)
	}

	pfAbsent := &ProjectFile{Config: FileConfig{}}
	cfgAbsent := Merge(pfAbsent, FlagOverrides{})
	if cfgAbsent.CompactThreshold != 0.85 || cfgAbsent.CompactKeepRecentMessages != 12 || cfgAbsent.MaxToolResultBytes != 262144 || cfgAbsent.CompactReserveTokens != 4000 {
		t.Fatalf("absent keys should fall back to defaults: threshold=%v keep=%d max=%d reserve=%d",
			cfgAbsent.CompactThreshold, cfgAbsent.CompactKeepRecentMessages, cfgAbsent.MaxToolResultBytes, cfgAbsent.CompactReserveTokens)
	}
}
