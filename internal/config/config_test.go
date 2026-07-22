package config

import "strings"
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

func TestTreeSitterEnabled(t *testing.T) {
	if !((*Config)(nil)).TreeSitterEnabled() {
		t.Fatal("nil config should enable tree-sitter (default on)")
	}
	def := Defaults()
	if !def.TreeSitterEnabled() {
		t.Fatal("default tree-sitter should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true", "", "random"} {
		c := Config{TreeSitter: on}
		if !c.TreeSitterEnabled() {
			t.Fatalf("TreeSitter=%q should enable (default on)", on)
		}
	}
	for _, off := range []string{"off", "OFF", "0", "false"} {
		c := Config{TreeSitter: off}
		if c.TreeSitterEnabled() {
			t.Fatalf("TreeSitter=%q should disable", off)
		}
	}
}

func TestWebFetchEnabled(t *testing.T) {
	if ((*Config)(nil)).WebFetchEnabled() {
		t.Fatal("nil config should not enable web fetch")
	}
	def := Defaults()
	if !def.WebFetchEnabled() {
		t.Fatal("default web fetch should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{WebFetch: on}
		if !c.WebFetchEnabled() {
			t.Fatalf("WebFetch=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{WebFetch: off}
		if c.WebFetchEnabled() {
			t.Fatalf("WebFetch=%q should not enable", off)
		}
	}
}

func TestWebSearchEnabled(t *testing.T) {
	if ((*Config)(nil)).WebSearchEnabled() {
		t.Fatal("nil config should not enable web search")
	}
	def := Defaults()
	if !def.WebSearchEnabled() {
		t.Fatal("default web search should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{WebSearch: on}
		if !c.WebSearchEnabled() {
			t.Fatalf("WebSearch=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{WebSearch: off}
		if c.WebSearchEnabled() {
			t.Fatalf("WebSearch=%q should not enable", off)
		}
	}
}

func TestWebToolsEnabled(t *testing.T) {
	if ((*Config)(nil)).WebToolsEnabled() {
		t.Fatal("nil config should not enable web tools")
	}

	tests := []struct {
		name   string
		fetch  string
		search string
		want   bool
	}{
		{"both off", "off", "off", false},
		{"fetch on", "on", "off", true},
		{"search on", "off", "on", true},
		{"both on", "on", "on", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{WebFetch: tt.fetch, WebSearch: tt.search}
			got := c.WebToolsEnabled()
			if got != tt.want {
				t.Errorf("WebToolsEnabled(fetch=%q, search=%q) = %v, want %v", tt.fetch, tt.search, got, tt.want)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	d := Defaults()
	if d.CompactThreshold != 0.75 {
		t.Errorf("default compact threshold = %f, want 0.75", d.CompactThreshold)
	}
	if d.MaxToolResultBytes != 8192 {
		t.Errorf("default max tool result bytes = %d, want 8192", d.MaxToolResultBytes)
	}
	if d.CommandTimeoutSecs != 120 {
		t.Errorf("default command timeout = %d, want 120", d.CommandTimeoutSecs)
	}
	if d.SessionMaxCount != 50 {
		t.Errorf("default session max count = %d, want 50", d.SessionMaxCount)
	}
	if d.SessionMaxAgeDays != 30 {
		t.Errorf("default session max age days = %d, want 30", d.SessionMaxAgeDays)
	}
	if d.CommandSafetyMode != "blocklist" {
		t.Errorf("default command safety mode = %q, want blocklist", d.CommandSafetyMode)
	}
	if d.DeleteApproval != "required" {
		t.Errorf("default delete approval = %q, want required", d.DeleteApproval)
	}
	if strings.TrimSpace(d.OpenAIModel) != "" {
		t.Errorf("default openai model should be empty, got %q", d.OpenAIModel)
	}
}
