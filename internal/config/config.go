package config

import (
	"strings"
)

// MCPServerConfig describes one MCP stdio server entry.
type MCPServerConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type Config struct {
	OpenAIKey   string
	OpenAIModel string
	OpenAIURL   string
	WorkingDir  string

	ContextLimit         int
	CompactThreshold     float64
	KeepRecentMessages   int
	MaxToolResultBytes   int
	CompactReserveTokens int

	CommandSafetyMode string // blocklist, allowlist, off
	CommandAllowlist  string // comma-separated when allowlist mode

	DeleteApproval string // required, off

	TreeSitter      string
	TreeSitterLangs string
	CLIVerbose      bool
	DebugLog        string
	DebugSession    string
	MCP             string
	MCPServers      []MCPServerConfig

	// ProjectGuidelines is loaded from the markdown body of the project file.
	ProjectGuidelines string
	ProjectFilePath   string
	TestCommand       string
	LintCommand       string

	WebBind           string // listen address for --web (default 127.0.0.1:8080)
	WebAllowedOrigins string // comma-separated host allowlist; empty uses localhost defaults
	WebAuthToken      string // required for non-loopback binds; also GOGEN_WEB_TOKEN

	WebFetch           string // on, off
	WebSearch          string // on, off
	WebSearchBackend   string // brave or "" for ddg
	WebSearchAPIKey    string // Brave API key
	WebAllowedDomains  string // comma-separated domain suffix allowlist
	WebFetchMode       string // https, all
}

// Defaults returns built-in default configuration values.
func Defaults() Config {
	return Config{
		OpenAIKey:            "",
		OpenAIModel:          "",
		OpenAIURL:            "",
		WorkingDir:           ".",
		ContextLimit:         0,
		CompactThreshold:     0.75,
		KeepRecentMessages:   12,
		MaxToolResultBytes:   8192,
		CompactReserveTokens: 4000,
		CommandSafetyMode:    "blocklist",
		CommandAllowlist:     "",
		DeleteApproval:       "required",
		TreeSitter:           "on",
		TreeSitterLangs:      "",
		CLIVerbose:           false,
		DebugLog:             "",
		DebugSession:         "",
		MCP:                  "on",
		WebBind:              "127.0.0.1:8080",
		WebAllowedOrigins:    "",
		WebFetch:             "on",
		WebSearch:            "on",
		WebSearchBackend:     "",
		WebSearchAPIKey:      "",
		WebAllowedDomains:    "",
		WebFetchMode:         "https",
	}
}

// MCPEnabled reports whether MCP integration is active.
func (c *Config) MCPEnabled() bool {
	if c == nil {
		return true
	}
	v := strings.ToLower(strings.TrimSpace(c.MCP))
	return v != "off" && v != "0" && v != "false"
}

// TreeSitterEnabled reports whether tree-sitter checks are active.
func (c *Config) TreeSitterEnabled() bool {
	if c == nil {
		return true
	}
	v := strings.ToLower(strings.TrimSpace(c.TreeSitter))
	return v != "off" && v != "0" && v != "false"
}

// WebFetchEnabled reports whether the web_fetch tool is active.
func (c *Config) WebFetchEnabled() bool {
	if c == nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(c.WebFetch))
	return v == "on" || v == "1" || v == "true"
}

// WebSearchEnabled reports whether the web_search tool is active.
func (c *Config) WebSearchEnabled() bool {
	if c == nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(c.WebSearch))
	return v == "on" || v == "1" || v == "true"
}

// WebToolsEnabled reports whether either web tool may use the network.
func (c *Config) WebToolsEnabled() bool {
	return c.WebFetchEnabled() || c.WebSearchEnabled()
}
