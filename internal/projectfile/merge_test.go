package projectfile

import (
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/config"
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

// session_max_age_days -1 is the "keep sessions forever" sentinel: unlike
// every other int option (where <= 0 means "use the default"), a negative
// file value must survive Merge so the store keeps its retention sentinel
// across restarts.
func TestMergePreservesNegativeSessionMaxAgeDays(t *testing.T) {
	os.Unsetenv("GOGEN_SESSION_MAX_AGE_DAYS")
	pf := &ProjectFile{Config: FileConfig{SessionMaxAgeDays: -1}}
	if got := Merge(pf, FlagOverrides{}).SessionMaxAgeDays; got != -1 {
		t.Fatalf("file -1 merged to %d, want -1 (keep forever)", got)
	}
	pfAbsent := &ProjectFile{Config: FileConfig{}}
	if got := Merge(pfAbsent, FlagOverrides{}).SessionMaxAgeDays; got != config.DefaultSessionMaxAgeDays {
		t.Fatalf("absent key merged to %d, want default %d", got, config.DefaultSessionMaxAgeDays)
	}
	// The env override still wins (and a positive env value is unchanged).
	t.Setenv("GOGEN_SESSION_MAX_AGE_DAYS", "30")
	if got := Merge(pf, FlagOverrides{}).SessionMaxAgeDays; got != 30 {
		t.Fatalf("env 30 merged to %d, want 30", got)
	}
}

// The board/subagent feature flags follow the opt-in MCP pattern: env >
// file > defaults, with GOGEN_SUBAGENT_MAX_DEPTH falling back to the default
// when unset/zero.
func TestMergeBoardSubagentFlags(t *testing.T) {
	for _, env := range []string{"GOGEN_BOARD", "GOGEN_SUBAGENT", "GOGEN_SUBAGENT_MAX_DEPTH"} {
		os.Unsetenv(env)
	}

	// Defaults: both off, depth default 1.
	cfg := Merge(nil, FlagOverrides{})
	if cfg.BoardEnabled() || cfg.SubagentEnabled() {
		t.Fatalf("defaults should be off: board=%q subagent=%q", cfg.Board, cfg.Subagent)
	}
	if cfg.SubagentDepth() != config.DefaultSubagentMaxDepth {
		t.Fatalf("default depth = %d, want %d", cfg.SubagentDepth(), config.DefaultSubagentMaxDepth)
	}

	// File values.
	pf := &ProjectFile{Config: FileConfig{Board: "on", Subagent: "on", SubagentMaxDepth: 3}}
	cfgFile := Merge(pf, FlagOverrides{})
	if !cfgFile.BoardEnabled() || !cfgFile.SubagentEnabled() {
		t.Fatalf("file flags not honored: board=%q subagent=%q", cfgFile.Board, cfgFile.Subagent)
	}
	if cfgFile.SubagentDepth() != 3 {
		t.Fatalf("file depth = %d, want 3", cfgFile.SubagentDepth())
	}

	// Env overrides file.
	t.Setenv("GOGEN_BOARD", "off")
	t.Setenv("GOGEN_SUBAGENT", "off")
	t.Setenv("GOGEN_SUBAGENT_MAX_DEPTH", "5")
	cfgEnv := Merge(pf, FlagOverrides{})
	if cfgEnv.BoardEnabled() || cfgEnv.SubagentEnabled() {
		t.Fatalf("env off should override file on: board=%q subagent=%q", cfgEnv.Board, cfgEnv.Subagent)
	}
	if cfgEnv.SubagentDepth() != 5 {
		t.Fatalf("env depth = %d, want 5", cfgEnv.SubagentDepth())
	}

	// Invalid depth env falls back to default.
	t.Setenv("GOGEN_SUBAGENT_MAX_DEPTH", "bogus")
	cfgBad := Merge(nil, FlagOverrides{})
	if cfgBad.SubagentDepth() != config.DefaultSubagentMaxDepth {
		t.Fatalf("invalid env depth = %d, want default %d", cfgBad.SubagentDepth(), config.DefaultSubagentMaxDepth)
	}
}

// --save-config must round-trip the new keys so a live toggle survives a
// restart.
func TestSaveConfigRoundTripsBoardSubagent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := config.Defaults()
	cfg.Board = "on"
	cfg.Subagent = "on"
	cfg.SubagentMaxDepth = 4
	cfg.SubagentModel = "gpt-4o-mini"
	if err := SaveConfig(cfgPath, "", &cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	pf, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(pf, FlagOverrides{})
	if !merged.BoardEnabled() || !merged.SubagentEnabled() {
		t.Fatalf("round-trip lost flags: board=%q subagent=%q", merged.Board, merged.Subagent)
	}
	if merged.SubagentDepth() != 4 {
		t.Fatalf("round-trip depth = %d, want 4", merged.SubagentDepth())
	}
	if merged.SubagentModel != "gpt-4o-mini" {
		t.Fatalf("round-trip subagent_model = %q, want gpt-4o-mini", merged.SubagentModel)
	}
}

// TestMergeSubagentModelPrecedence pins the subagent default model merge:
// empty (default) = inherit, file value applies, env overrides file.
func TestMergeSubagentModelPrecedence(t *testing.T) {
	os.Unsetenv("GOGEN_SUBAGENT_MODEL")
	if got := Merge(nil, FlagOverrides{}).SubagentModel; got != "" {
		t.Fatalf("default subagent_model = %q, want empty (inherit)", got)
	}
	pf := &ProjectFile{Config: FileConfig{SubagentModel: "llama3.1"}}
	if got := Merge(pf, FlagOverrides{}).SubagentModel; got != "llama3.1" {
		t.Fatalf("file subagent_model = %q, want llama3.1", got)
	}
	t.Setenv("GOGEN_SUBAGENT_MODEL", "qwen2.5")
	if got := Merge(pf, FlagOverrides{}).SubagentModel; got != "qwen2.5" {
		t.Fatalf("env subagent_model = %q, want qwen2.5", got)
	}
}

// TestMergePromptTemplates pins the configurable prompt template merge:
// empty = the built-in default, file value applies, env overrides file.
func TestMergePromptTemplates(t *testing.T) {
	os.Unsetenv("GOGEN_BOARD_START_PROMPT")
	os.Unsetenv("GOGEN_SYSTEM_PROMPT")
	os.Unsetenv("GOGEN_SUBAGENT_PROMPT")
	if got := Merge(nil, FlagOverrides{}).BoardStartPrompt; got != "" {
		t.Fatalf("default board_start_prompt = %q, want empty (built-in)", got)
	}
	pf := &ProjectFile{Config: FileConfig{
		BoardStartPrompt: "board {title}",
		SystemPrompt:     "sys {working_dir}",
		SubagentPrompt:   "sub {job}",
	}}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.BoardStartPrompt != "board {title}" || cfg.SystemPrompt != "sys {working_dir}" || cfg.SubagentPrompt != "sub {job}" {
		t.Fatalf("file prompt templates = %+v", cfg)
	}
	t.Setenv("GOGEN_SYSTEM_PROMPT", "env {working_dir}")
	if got := Merge(pf, FlagOverrides{}).SystemPrompt; got != "env {working_dir}" {
		t.Fatalf("env system_prompt = %q, want env value", got)
	}
}

// ConfigFileHasSecrets must detect stored secrets in every config file
// shape the toggle persist can rewrite: .conf, the plain-YAML global config
// (config.yaml — no front matter), and front-matter .md files. Missing it
// would silently drop openai_api_key / MCP envs on a live toggle.
func TestConfigFileHasSecrets(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "gogen.conf")
	if err := os.WriteFile(confPath, []byte("openai_api_key: sk-conf\nopenai_model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(confPath) {
		t.Fatal(".conf with a key should report secrets")
	}

	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("openai_api_key: sk-yaml\nopenai_model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(yamlPath) {
		t.Fatal("plain-YAML global config with a key should report secrets")
	}

	mdPath := filepath.Join(dir, "GOGEN.md")
	md := "---\nopenai_api_key: sk-md\nopenai_model: m\n---\n# rules\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(mdPath) {
		t.Fatal("front-matter .md with a key should report secrets")
	}

	envPath := filepath.Join(dir, "env.conf")
	if err := os.WriteFile(envPath, []byte("openai_model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ConfigFileHasSecrets(envPath) {
		t.Fatal("config without secrets should report none")
	}
	if ConfigFileHasSecrets(filepath.Join(dir, "missing.conf")) {
		t.Fatal("missing config should report no secrets")
	}

	mcpPath := filepath.Join(dir, "mcp.conf")
	mcp := "mcp: on\nmcp_servers:\n  - name: fetch\n    command: npx\n    args: [\"-y\", \"@modelcontextprotocol/server-fetch\"]\n    env:\n      TOKEN: secret\n"
	if err := os.WriteFile(mcpPath, []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(mcpPath) {
		t.Fatal("MCP server env should report secrets")
	}
}

// An existing but unparseable config is conservatively treated as having
// secrets so a toggle rewrite cannot drop a key from a file it cannot read.
func TestConfigFileHasSecretsUnparseableAssumesSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.conf")
	if err := os.WriteFile(path, []byte("{not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(path) {
		t.Fatal("unparseable existing config should conservatively report secrets")
	}
}
