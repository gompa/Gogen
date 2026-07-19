package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.Contains(text, "OPENAI_API_KEY") {
		t.Fatalf("expected env comment: %q", text)
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

func TestCommandAllowlistList(t *testing.T) {
	pf, err := ParseContent("GOGEN.md", "---\ncommand_allowlist: [go, git, make]\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Config.CommandAllowlist != "go,git,make" {
		t.Fatalf("got %q", pf.Config.CommandAllowlist)
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
