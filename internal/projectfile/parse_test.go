package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/config"
)

func TestParseRulesOnly(t *testing.T) {
	pf, err := ParseContent("GOGEN.md", "# Rules\n\nDo things.\n")
	if err != nil {
		t.Fatal(err)
	}
	if pf.HasConfig {
		t.Fatal("expected no config")
	}
	if !strings.Contains(pf.Guidelines, "Do things") {
		t.Fatalf("guidelines: %q", pf.Guidelines)
	}
}

func TestParseFrontMatter(t *testing.T) {
	content := "---\ncommand_safety: off\n---\n# Rules\n"
	pf, err := ParseContent("GOGEN.md", content)
	if err != nil {
		t.Fatal(err)
	}
	if !pf.HasConfig {
		t.Fatal("expected config")
	}
	if pf.Config.CommandSafety != "off" {
		t.Fatalf("command_safety=%q", pf.Config.CommandSafety)
	}
	if pf.Guidelines != "# Rules" {
		t.Fatalf("guidelines=%q", pf.Guidelines)
	}
}

// The pre-rename keep_recent_messages key must keep working (value AND
// explicit 0), with the current key winning when both are present, and the
// legacy field must be cleared after parsing so it never leaks elsewhere.
func TestParseLegacyKeepRecentMessagesKey(t *testing.T) {
	pf, err := ParseContent("GOGEN.md", "---\nkeep_recent_messages: 20\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Config.CompactKeepRecentMessages == nil || *pf.Config.CompactKeepRecentMessages != 20 {
		t.Fatalf("legacy key not aliased: CompactKeepRecentMessages = %v, want 20", pf.Config.CompactKeepRecentMessages)
	}
	if pf.Config.KeepRecentMessages != nil {
		t.Fatalf("legacy field not cleared after aliasing: %v", *pf.Config.KeepRecentMessages)
	}

	// Explicit 0 under the legacy key is a real setting and must survive.
	pfZero, err := ParseContent("GOGEN.md", "---\nkeep_recent_messages: 0\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if pfZero.Config.CompactKeepRecentMessages == nil || *pfZero.Config.CompactKeepRecentMessages != 0 {
		t.Fatalf("legacy explicit 0 lost: CompactKeepRecentMessages = %v, want 0", pfZero.Config.CompactKeepRecentMessages)
	}

	// Current key wins when both are present.
	pfBoth, err := ParseContent("GOGEN.md", "---\nkeep_recent_messages: 5\ncompact_keep_recent_messages: 8\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if pfBoth.Config.CompactKeepRecentMessages == nil || *pfBoth.Config.CompactKeepRecentMessages != 8 {
		t.Fatalf("current key should win: CompactKeepRecentMessages = %v, want 8", pfBoth.Config.CompactKeepRecentMessages)
	}

	// The alias also applies to pure-YAML .conf files.
	cfg, err := ParseConfigFile("gogen.conf", "keep_recent_messages: 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Config.CompactKeepRecentMessages == nil || *cfg.Config.CompactKeepRecentMessages != 3 {
		t.Fatalf(".conf legacy key not aliased: CompactKeepRecentMessages = %v, want 3", cfg.Config.CompactKeepRecentMessages)
	}

	// End-to-end through Merge: the aliased value reaches the runtime config.
	merged := Merge(pf, FlagOverrides{})
	if merged.CompactKeepRecentMessages != 20 {
		t.Fatalf("Merge did not carry the aliased legacy value: %d, want 20", merged.CompactKeepRecentMessages)
	}
}

// The legacy key must not be re-emitted by --save-config regeneration: the
// writer renders a separate projection struct, but the aliased parse result
// must also not carry the stale field forward.
func TestParseLegacyKeyNotWritten(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := Merge(&ProjectFile{Config: FileConfig{}}, FlagOverrides{})
	cfg.CompactKeepRecentMessages = 4
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	// The generated config legitimately contains compact_keep_recent_messages;
	// only the bare legacy key (a line of its own) must not appear.
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "keep_recent_messages:") {
			t.Fatalf("legacy key leaked into saved config:\n%s", text)
		}
	}
}

func TestParseMissingClosingDelimiter(t *testing.T) {
	_, err := ParseContent("GOGEN.md", "---\ncommand_safety: off\n# no close")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMergeEnvOverridesFile(t *testing.T) {
	t.Setenv("GOGEN_COMMAND_SAFETY", "blocklist")
	pf, err := ParseContent("GOGEN.md", "---\ncommand_safety: off\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CommandSafetyMode != "blocklist" {
		t.Fatalf("got %q", cfg.CommandSafetyMode)
	}
}

func TestMergeFileValueWhenEnvUnset(t *testing.T) {
	os.Unsetenv("GOGEN_CONTEXT_LIMIT")
	pf, err := ParseContent("GOGEN.md", "---\ncontext_limit: 128000\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.ContextLimit != 128000 {
		t.Fatalf("got %d", cfg.ContextLimit)
	}
}

func TestMergeEmptyEnvClearsBaseURL(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	pf, err := ParseContent("GOGEN.md", "---\nopenai_base_url: https://example.com\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.OpenAIURL != "" {
		t.Fatalf("got %q", cfg.OpenAIURL)
	}
}

func TestMergePreserveReasoning(t *testing.T) {
	os.Unsetenv("GOGEN_PRESERVE_REASONING")
	pf, err := ParseContent("GOGEN.md", "---\npreserve_reasoning: on\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.PreserveReasoning != "on" {
		t.Fatalf("file value: got %q", cfg.PreserveReasoning)
	}

	t.Setenv("GOGEN_PRESERVE_REASONING", "off")
	cfg = Merge(pf, FlagOverrides{})
	if cfg.PreserveReasoning != "off" {
		t.Fatalf("env override: got %q", cfg.PreserveReasoning)
	}

	os.Unsetenv("GOGEN_PRESERVE_REASONING")
	cfg = Merge(nil, FlagOverrides{})
	if cfg.PreserveReasoning != "auto" {
		t.Fatalf("default: got %q", cfg.PreserveReasoning)
	}
}

func TestSaveConfigRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".gogen", "gogen.conf")
	mdPath := filepath.Join(dir, ".gogen", "gogen.md")
	cfg := Merge(nil, FlagOverrides{})
	cfg.OpenAIKey = "sk-secret"
	cfg.OpenAIModel = "gpt-4o"
	if err := SaveConfig(cfgPath, mdPath, cfg, "# Rules", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "sk-secret") {
		t.Fatalf("secret leaked: %q", text)
	}
	if strings.Contains(text, "openai_api_key") {
		t.Fatalf("expected openai_api_key omitted without --save-config-secrets: %q", text)
	}
}

func TestSaveConfigWithoutAPIKey(t *testing.T) {
	os.Unsetenv("OPENAI_API_KEY")
	dir := t.TempDir()
	cfgPath := DefaultSavePath(dir)
	mdPath := DefaultGuidelinesSavePath(dir)
	cfg := Merge(nil, FlagOverrides{})
	if err := SaveConfig(cfgPath, mdPath, cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// The writer must never drop a 0 on the four context settings: each 0 is a
// real setting (auto-compaction off, no recent messages kept, no truncation
// cap, no reserved tokens), so regeneration must round-trip it.
func TestSaveConfigPreservesExplicitZeros(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zero.conf")
	cfg := Merge(nil, FlagOverrides{})
	cfg.CompactThreshold = 0
	cfg.CompactKeepRecentMessages = 0
	cfg.MaxToolResultBytes = 0
	cfg.CompactReserveTokens = 0
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, key := range []string{
		"compact_threshold: 0",
		"compact_keep_recent_messages: 0",
		"max_tool_result_bytes: 0",
		"compact_reserve_tokens: 0",
	} {
		if !strings.Contains(text, key) {
			t.Fatalf("expected %q in generated config:\n%s", key, text)
		}
	}
}

// Pins the overall shape of generated configs: pure YAML (no --- marker),
// curated key order, and mcp_servers rendered through the YAML encoder.
func TestSaveConfigOutputShape(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := Merge(nil, FlagOverrides{})
	cfg.OpenAIModel = "gpt-4o"
	cfg.OpenAIURL = "https://api.openai.com/v1"
	cfg.WorkingDir = "/home/user/proj"
	cfg.MCPServers = []config.MCPServerConfig{{Name: "fetch", Command: "npx", Args: []string{"-y", "server"}}}
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{IncludeSecrets: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.HasPrefix(text, "---") {
		t.Fatalf("expected pure YAML without front-matter marker:\n%s", text)
	}
	for _, want := range []string{
		"openai_model: gpt-4o",
		"openai_base_url:",
		"working_dir: /home/user/proj",
		"compact_threshold: 0.85",
		"mcp_servers:",
		"  - name: fetch",
		"    command: npx",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in output:\n%s", want, text)
		}
	}
}

func TestSaveConfigPreserveReasoning(t *testing.T) {
	dir := t.TempDir()

	cfg := Merge(nil, FlagOverrides{})
	cfgPath := filepath.Join(dir, "auto.conf")
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "preserve_reasoning") {
		t.Fatalf("default auto should be omitted: %s", data)
	}

	cfg.PreserveReasoning = "on"
	cfgPath = filepath.Join(dir, "on.conf")
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "preserve_reasoning: on") &&
		!strings.Contains(string(data), `preserve_reasoning: "on"`) {
		t.Fatalf("expected on override: %s", data)
	}
}

// List-form allowlists are rejected: the schema requires a comma-separated
// string, and yaml.v3 cannot unmarshal a sequence into a string field.
func TestCommandAllowlistListRejected(t *testing.T) {
	_, err := ParseContent("GOGEN.md", "---\ncommand_allowlist: [go, git, make]\n---\n")
	if err == nil {
		t.Fatal("expected list-form command_allowlist to be rejected")
	}
}

// Explicit zeros for the pointer-typed fields must survive parsing so the
// merge layer can distinguish them from absent keys.
func TestParseZeroValuedFieldsPreserved(t *testing.T) {
	pf, err := ParseContent("GOGEN.md", "---\ncompact_keep_recent_messages: 0\ncompact_threshold: 0\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Config.CompactKeepRecentMessages == nil || *pf.Config.CompactKeepRecentMessages != 0 {
		t.Fatalf("compact_keep_recent_messages should be explicit 0, got %v", pf.Config.CompactKeepRecentMessages)
	}
	if pf.Config.CompactThreshold == nil || *pf.Config.CompactThreshold != 0 {
		t.Fatalf("compact_threshold should be explicit 0, got %v", pf.Config.CompactThreshold)
	}
}

func TestParseAbsentPointerFieldsNil(t *testing.T) {
	pf, err := ParseContent("GOGEN.md", "---\ncommand_safety: off\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Config.CompactKeepRecentMessages != nil {
		t.Fatalf("absent compact_keep_recent_messages should be nil, got %d", *pf.Config.CompactKeepRecentMessages)
	}
	if pf.Config.CompactThreshold != nil {
		t.Fatalf("absent compact_threshold should be nil, got %f", *pf.Config.CompactThreshold)
	}
}

func TestParseMCPServersRequiresNameAndCommand(t *testing.T) {
	_, err := ParseContent("GOGEN.md", "---\nmcp_servers:\n  - command: npx\n---\n")
	if err == nil {
		t.Fatal("expected error for mcp server entry missing name")
	}
}

func TestDiscoverPriority(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "GOGEN.md"), []byte("# root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".gogen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gogen", "gogen.md"), []byte("# canonical"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, ok := DiscoverConfigPath(dir)
	if !ok {
		t.Fatal("expected config file")
	}
	if !strings.HasSuffix(path, filepath.Join(".gogen", "gogen.md")) {
		t.Fatalf("got %q", path)
	}
}

func TestExtractMarkdownBodyMatchesParseContent(t *testing.T) {
	cases := []string{
		"---\ncommand_safety: off\n---\n# Rules\n",
		"---\ncommand_safety: off\n---", // closing --- at EOF, no trailing newline
		"---\r\ncommand_safety: off\r\n---\r\n# Rules\r\n",
		"# plain guidelines\n",
		"---\nno closing delimiter",
	}
	for _, content := range cases {
		body := extractMarkdownBody(content)
		pf, err := ParseContent("GOGEN.md", content)
		if err != nil {
			if body != "" {
				t.Fatalf("ParseContent failed but extractMarkdownBody=%q for %q: %v", body, content, err)
			}
			continue
		}
		want := pf.Guidelines
		if body != want {
			t.Fatalf("extractMarkdownBody=%q ParseContent.Guidelines=%q for %q", body, want, content)
		}
	}
}
