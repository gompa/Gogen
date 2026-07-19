package config

import "testing"

func TestMCPEnabledOptIn(t *testing.T) {
	if ((*Config)(nil)).MCPEnabled() {
		t.Fatal("nil config should not enable MCP")
	}
	def := Defaults()
	if def.MCPEnabled() {
		t.Fatal("default MCP should be off")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{MCP: on}
		if !c.MCPEnabled() {
			t.Fatalf("MCP=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{MCP: off}
		if c.MCPEnabled() {
			t.Fatalf("MCP=%q should not enable", off)
		}
	}
}
