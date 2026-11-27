package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/config"
)

// TestMCPProbeSettingsRoundTrip pins the two settings-modal MCP test bounds in
// the config-file layer: `mcp_probe_timeout_secs` / `mcp_probe_max_concurrent`
// are read from the project file, overridden by their env vars, survive Merge
// into the runtime config, and are re-emitted by --save-config once they differ
// from the built-in default (they are not secrets).
func TestMCPProbeSettingsRoundTrip(t *testing.T) {
	body := "mcp: on\nmcp_probe_timeout_secs: 1800\nmcp_probe_max_concurrent: 4\n"
	pf, err := ParseConfigFile("gogen.conf", body)
	if err != nil {
		t.Fatalf("ParseConfigFile: %v", err)
	}

	os.Unsetenv("GOGEN_MCP_PROBE_TIMEOUT_SECS")
	os.Unsetenv("GOGEN_MCP_PROBE_MAX_CONCURRENT")
	cfg := Merge(pf, FlagOverrides{})
	if cfg.MCPProbeTimeoutSecs != 1800 || cfg.MCPProbeMaxConcurrent != 4 {
		t.Fatalf("file values = %d / %d, want 1800 / 4",
			cfg.MCPProbeTimeoutSecs, cfg.MCPProbeMaxConcurrent)
	}

	// The env vars win over the file, so a script can set them.
	t.Setenv("GOGEN_MCP_PROBE_TIMEOUT_SECS", "300")
	t.Setenv("GOGEN_MCP_PROBE_MAX_CONCURRENT", "1")
	cfg = Merge(pf, FlagOverrides{})
	if cfg.MCPProbeTimeoutSecs != 300 || cfg.MCPProbeMaxConcurrent != 1 {
		t.Fatalf("env values = %d / %d, want 300 / 1",
			cfg.MCPProbeTimeoutSecs, cfg.MCPProbeMaxConcurrent)
	}

	// --save-config re-emits both keys with their non-default values.
	path := filepath.Join(t.TempDir(), "gogen.conf")
	if err := SaveConfig(path, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mcp_probe_timeout_secs: 300", "mcp_probe_max_concurrent: 1"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("saved config missing %q:\n%s", want, data)
		}
	}
}

// TestMCPProbeSettingsOmittedAtDefault pins the "absent = default" convention:
// a config that never set the keys does not bake the defaults into a
// regenerated file, so a future default change reaches users who never
// customized them.
func TestMCPProbeSettingsOmittedAtDefault(t *testing.T) {
	os.Unsetenv("GOGEN_MCP_PROBE_TIMEOUT_SECS")
	os.Unsetenv("GOGEN_MCP_PROBE_MAX_CONCURRENT")
	def := config.Defaults()
	if got := def.MCPProbeTimeoutSecs; got != config.DefaultMCPProbeTimeoutSecs {
		t.Fatalf("Defaults().MCPProbeTimeoutSecs = %d, want %d", got, config.DefaultMCPProbeTimeoutSecs)
	}
	path := filepath.Join(t.TempDir(), "gogen.conf")
	if err := SaveConfig(path, "", &def, "", WriteOptions{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mcp_probe_timeout_secs", "mcp_probe_max_concurrent"} {
		if strings.Contains(string(data), key) {
			t.Fatalf("saved config baked in the default %q:\n%s", key, data)
		}
	}
}
