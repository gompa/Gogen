package config

import (
	"strings"
)

// Exported defaults for configurable settings. Other packages reference these
// constants instead of duplicating literals, so the values have a single
// source of truth.
const (
	// DefaultCompactThreshold is the fraction of the context limit that
	// triggers auto-compaction.
	DefaultCompactThreshold = 0.75
	// DefaultKeepRecentMessages is the number of recent messages preserved
	// during compaction.
	DefaultKeepRecentMessages = 12
	// DefaultMaxToolResultBytes is the cap for tool output before truncation
	// (256 KB — matches web_fetch's default body cap).
	DefaultMaxToolResultBytes = 262144
	// DefaultCompactReserveTokens is the token budget reserved for new
	// messages after compaction.
	DefaultCompactReserveTokens = 4000
	// DefaultCommandTimeoutSecs is the maximum duration for execute_command.
	DefaultCommandTimeoutSecs = 120
	// DefaultSessionMaxCount is the maximum saved sessions per working dir.
	DefaultSessionMaxCount = 50
	// DefaultSessionMaxAgeDays is the retention window for saved sessions.
	DefaultSessionMaxAgeDays = 30
	// DefaultContextLimit is the fallback context window size (tokens) when
	// it cannot be resolved from the provider.
	DefaultContextLimit = 128000
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

	// DebugCompareMessages enables view-fingerprint comparison across turns (GOGEN_DEBUG_COMPARE_MESSAGES).
	// Only effective in binaries built with `-tags debug`; ignored otherwise.
	DebugCompareMessages bool

	// ProjectGuidelines is loaded from the markdown body of the project file.
	ProjectGuidelines string
	ProjectFilePath   string
	TestCommand       string
	LintCommand       string

	WebBind           string // listen address for --web (default 127.0.0.1:8081)
	WebAllowedOrigins string // comma-separated host allowlist; empty uses localhost defaults
	WebAuthToken      string // required for non-loopback binds; also GOGEN_WEB_TOKEN
	WebTLSCertFile    string // PEM cert for TLS (required with key for non-loopback)
	WebTLSKeyFile     string // PEM key for TLS

	SessionMaxCount   int // max saved sessions per working dir (0 = default 50)
	SessionMaxAgeDays int // delete sessions older than N days (0 = default 30)

	WebFetch          string // on, off
	WebSearch         string // on, off
	WebSearchBackend  string // brave or "" for ddg
	WebSearchAPIKey   string // Brave API key
	WebAllowedDomains string // comma-separated domain suffix allowlist
	WebFetchMode      string // https, all

	CommandSandbox     string // off, bwrap (bubblewrap when available)
	CommandTimeoutSecs int    // execute_command timeout; 0 = default 120s

	// PreserveReasoning controls chat_template_kwargs.preserve_reasoning for
	// self-hosted OpenAI-compatible servers: auto (probe /props), on, off.
	PreserveReasoning string
}

// Defaults returns built-in default configuration values.
func Defaults() Config {
	return Config{
		OpenAIKey:            "",
		OpenAIModel:          "",
		OpenAIURL:            "",
		WorkingDir:           ".",
		ContextLimit:         0,
		CompactThreshold:     DefaultCompactThreshold,
		KeepRecentMessages:   DefaultKeepRecentMessages,
		MaxToolResultBytes:   DefaultMaxToolResultBytes,
		CompactReserveTokens: DefaultCompactReserveTokens,
		CommandSafetyMode:    "blocklist",
		CommandAllowlist:     "",
		DeleteApproval:       "required",
		TreeSitter:           "on",
		TreeSitterLangs:      "",
		CLIVerbose:           false,
		DebugLog:             "",
		DebugSession:         "",
		MCP:                  "off",
		DebugCompareMessages: false,
		WebBind:              "127.0.0.1:8081",
		WebAllowedOrigins:    "",
		WebTLSCertFile:       "",
		WebTLSKeyFile:        "",
		SessionMaxCount:      DefaultSessionMaxCount,
		SessionMaxAgeDays:    DefaultSessionMaxAgeDays,
		WebFetch:             "on",
		WebSearch:            "on",
		WebSearchBackend:     "",
		WebSearchAPIKey:      "",
		WebAllowedDomains:    "",
		WebFetchMode:         "https",
		CommandSandbox:       "off",
		CommandTimeoutSecs:   DefaultCommandTimeoutSecs,
		PreserveReasoning:    "auto",
	}
}

// configOn reports whether a config field is explicitly enabled.
func configOn(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "on" || v == "1" || v == "true"
}

// configOff reports whether a config field is explicitly disabled
// (set to "off", "0", or "false"). An empty string is not "off" —
// absence of a value does not imply explicit disablement.
func configOff(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "off" || v == "0" || v == "false"
}

// MCPEnabled reports whether MCP integration is active.
// Opt-in: servers in project config are not started unless mcp is explicitly enabled.
func (c *Config) MCPEnabled() bool {
	return c != nil && configOn(c.MCP)
}

// TreeSitterEnabled reports whether tree-sitter checks are active.
// Enabled by default (on unless explicitly set to "off", "0", or "false").
func (c *Config) TreeSitterEnabled() bool {
	if c == nil {
		return true
	}
	return !configOff(c.TreeSitter)
}

// WebFetchEnabled reports whether the web_fetch tool is active.
func (c *Config) WebFetchEnabled() bool {
	if c == nil {
		return false
	}
	return configOn(c.WebFetch)
}

// WebSearchEnabled reports whether the web_search tool is active.
func (c *Config) WebSearchEnabled() bool {
	if c == nil {
		return false
	}
	return configOn(c.WebSearch)
}

// WebToolsEnabled reports whether either web tool may use the network.
func (c *Config) WebToolsEnabled() bool {
	return c.WebFetchEnabled() || c.WebSearchEnabled()
}
