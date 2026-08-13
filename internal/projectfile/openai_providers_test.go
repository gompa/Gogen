package projectfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gogen/internal/config"
)

func TestParseOpenAIProviders(t *testing.T) {
	pf, err := ParseContent("GOGEN.md", `---
openai_providers:
  - name: local-llama
    base_url: http://127.0.0.1:8080/v1
    api_key: llama-key
    model: llama3.1
  - name: opencode-zen
    base_url: https://opencode.ai/zen/v1/
---
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []OpenAIProviderEntry{
		{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "llama-key", Model: "llama3.1"},
		{Name: "opencode-zen", BaseURL: "https://opencode.ai/zen/v1/"},
	}
	if !reflect.DeepEqual(pf.Config.OpenAIProviders, want) {
		t.Fatalf("parsed providers = %+v, want %+v", pf.Config.OpenAIProviders, want)
	}
}

// An entry without a name decodes without error (empty string), so the
// validator must reject it explicitly — mirroring validateMCPServers.
func TestParseOpenAIProvidersRequiresName(t *testing.T) {
	_, err := ParseContent("GOGEN.md", "---\nopenai_providers:\n  - base_url: http://127.0.0.1:8080/v1\n---\n")
	if err == nil {
		t.Fatal("expected error for provider entry missing name")
	}
}

// The file's openai_providers list flows through Merge into the runtime
// config (converted to config entries).
func TestMergeOpenAIProvidersFromFile(t *testing.T) {
	os.Unsetenv("GOGEN_OPENAI_PROVIDERS")
	pf, err := ParseContent("GOGEN.md", `---
openai_providers:
  - name: local-llama
    base_url: http://127.0.0.1:8080/v1
    api_key: llama-key
    model: llama3.1
---
`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	want := []config.OpenAIProviderConfig{
		{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "llama-key", Model: "llama3.1"},
	}
	if !reflect.DeepEqual(cfg.OpenAIProviders, want) {
		t.Fatalf("merged providers = %+v, want %+v", cfg.OpenAIProviders, want)
	}
}

// The GOGEN_OPENAI_PROVIDERS JSON env var wins over the file and is parsed
// with the config JSON tags (baseUrl, apiKey).
func TestMergeOpenAIProvidersFromEnv(t *testing.T) {
	t.Setenv("GOGEN_OPENAI_PROVIDERS", `[{"name":"env-provider","baseUrl":"https://example.com/v1","apiKey":"env-key","model":"env-model"}]`)
	pf, err := ParseContent("GOGEN.md", `---
openai_providers:
  - name: file-provider
    base_url: http://127.0.0.1:8080/v1
---
`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	want := []config.OpenAIProviderConfig{
		{Name: "env-provider", BaseURL: "https://example.com/v1", APIKey: "env-key", Model: "env-model"},
	}
	if !reflect.DeepEqual(cfg.OpenAIProviders, want) {
		t.Fatalf("env should override file: got %+v, want %+v", cfg.OpenAIProviders, want)
	}
}

// An explicitly empty env value clears the list (like GOGEN_MCP_SERVERS).
func TestMergeOpenAIProvidersEnvEmptyClears(t *testing.T) {
	t.Setenv("GOGEN_OPENAI_PROVIDERS", "")
	pf, err := ParseContent("GOGEN.md", "---\nopenai_providers:\n  - name: file-provider\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.OpenAIProviders != nil {
		t.Fatalf("empty env should clear the list, got %+v", cfg.OpenAIProviders)
	}
}

// Invalid JSON env is ignored with a warning and yields an empty list —
// exactly the GOGEN_MCP_SERVERS semantics (env present but unusable wins,
// so a broken value never silently falls back to the file).
func TestMergeOpenAIProvidersEnvInvalidJSON(t *testing.T) {
	t.Setenv("GOGEN_OPENAI_PROVIDERS", "not-json")
	pf, err := ParseContent("GOGEN.md", "---\nopenai_providers:\n  - name: file-provider\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.OpenAIProviders != nil {
		t.Fatalf("invalid env should clear the list (MCP semantics), got %+v", cfg.OpenAIProviders)
	}
}

// Provider API keys must be redacted without IncludeSecrets, exactly like
// openai_api_key and MCP server env: the list is still written (names and
// base URLs are not secrets), but api_key is omitted.
func TestSaveConfigOpenAIProvidersRedactsKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := Merge(nil, FlagOverrides{})
	cfg.OpenAIProviders = []config.OpenAIProviderConfig{
		{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "sk-provider-secret", Model: "llama3.1"},
	}
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"openai_providers:", "name: local-llama", "base_url: http://127.0.0.1:8080/v1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in output without secrets:\n%s", want, text)
		}
	}
	for _, leak := range []string{"sk-provider-secret", "api_key:"} {
		if strings.Contains(text, leak) {
			t.Fatalf("provider api_key leaked without --save-config-secrets (%q):\n%s", leak, text)
		}
	}

	// With IncludeSecrets the key is persisted.
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{IncludeSecrets: true}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sk-provider-secret", "api_key:"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected %q in output with IncludeSecrets:\n%s", want, data)
		}
	}
}

// Save → reload → merge round-trips the full provider list including keys
// (the web persistConfig path: IncludeSecrets forced when the file already
// has secrets).
func TestSaveConfigOpenAIProvidersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := Merge(nil, FlagOverrides{})
	cfg.OpenAIProviders = []config.OpenAIProviderConfig{
		{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "llama-key", Model: "llama3.1"},
		{Name: "opencode-zen", BaseURL: "https://opencode.ai/zen/v1/"},
	}
	if err := SaveConfig(cfgPath, "", cfg, "", WriteOptions{IncludeSecrets: true}); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("GOGEN_OPENAI_PROVIDERS")
	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(loaded, FlagOverrides{})
	if !reflect.DeepEqual(merged.OpenAIProviders, cfg.OpenAIProviders) {
		t.Fatalf("round-trip providers = %+v, want %+v", merged.OpenAIProviders, cfg.OpenAIProviders)
	}
}
