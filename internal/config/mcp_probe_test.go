package config

import (
	"testing"
	"time"
)

// TestMCPProbeTimeoutCeiling pins the resolved ceiling on a settings-modal MCP
// connection test (mcp_probe_timeout_secs). 0 and negative mean "use the
// built-in default"; a configured value wins; an absurd value is clamped to a
// day so the seconds→Duration multiply cannot overflow into a negative (which
// would have resolved to the probe floor, the opposite of the request).
func TestMCPProbeTimeoutCeiling(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		secs int
		want time.Duration
	}{
		{name: "nil config uses the default", cfg: nil, want: DefaultMCPProbeTimeoutSecs * time.Second},
		{name: "unset uses the default", secs: 0, want: DefaultMCPProbeTimeoutSecs * time.Second},
		{name: "negative uses the default", secs: -30, want: DefaultMCPProbeTimeoutSecs * time.Second},
		{name: "a raised value is honored", secs: 1800, want: 1800 * time.Second},
		{name: "a lowered value is honored", secs: 30, want: 30 * time.Second},
		{name: "an absurd value is clamped to a day", secs: 1 << 40, want: 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg == nil && tc.name != "nil config uses the default" {
				cfg = &Config{MCPProbeTimeoutSecs: tc.secs}
			}
			if got := cfg.MCPProbeTimeoutCeiling(); got != tc.want {
				t.Fatalf("MCPProbeTimeoutCeiling(%d) = %v, want %v", tc.secs, got, tc.want)
			}
		})
	}
}

// TestMCPProbeConcurrency pins the resolved concurrent-test cap
// (mcp_probe_max_concurrent): 0 and negative mean the built-in default, and a
// configured value is used as it is.
func TestMCPProbeConcurrency(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want int
	}{
		{name: "nil config uses the default", cfg: nil, want: DefaultMCPProbeMaxConcurrent},
		{name: "unset uses the default", cfg: &Config{}, want: DefaultMCPProbeMaxConcurrent},
		{name: "negative uses the default", cfg: &Config{MCPProbeMaxConcurrent: -1}, want: DefaultMCPProbeMaxConcurrent},
		{name: "a configured value is used", cfg: &Config{MCPProbeMaxConcurrent: 1}, want: 1},
		{name: "a raised value is used", cfg: &Config{MCPProbeMaxConcurrent: 8}, want: 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.MCPProbeConcurrency(); got != tc.want {
				t.Fatalf("MCPProbeConcurrency() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMCPProbeDefaultsAreTheBuiltins pins the two defaults the settings and the
// probe documented before they were configurable: a ten-minute ceiling and two
// concurrent tests. A changed constant must be a deliberate act, because both
// are described in the README and the settings modal.
func TestMCPProbeDefaultsAreTheBuiltins(t *testing.T) {
	def := Defaults()
	if def.MCPProbeTimeoutSecs != 600 {
		t.Fatalf("default mcp_probe_timeout_secs = %d, want 600", def.MCPProbeTimeoutSecs)
	}
	if def.MCPProbeMaxConcurrent != 2 {
		t.Fatalf("default mcp_probe_max_concurrent = %d, want 2", def.MCPProbeMaxConcurrent)
	}
	if got := def.MCPProbeTimeoutCeiling(); got != 10*time.Minute {
		t.Fatalf("default probe ceiling = %v, want 10m", got)
	}
	if got := def.MCPProbeConcurrency(); got != 2 {
		t.Fatalf("default probe concurrency = %d, want 2", got)
	}
}
