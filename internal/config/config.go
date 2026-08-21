package config

import (
	"time"

	"gogen/internal/onoff"
)

// Exported defaults for configurable settings. Other packages reference these
// constants instead of duplicating literals, so the values have a single
// source of truth.
const (
	// DefaultCompactThreshold is the fraction of the context limit that
	// triggers auto-compaction. 0.85 leaves less headroom before the window
	// fills than the old 0.75, but the continuation-summary request is a
	// cache-friendly prefix (see contextmgr.summarizeMiddle), so compacting
	// later is cheap and the headroom is covered by the reserve.
	DefaultCompactThreshold = 0.85
	// DefaultCompactKeepRecentMessages is the number of recent messages
	// preserved verbatim during compaction.
	DefaultCompactKeepRecentMessages = 12
	// DefaultMaxToolResultBytes is the cap for tool output before truncation
	// (256 KB — matches web_fetch's default body cap).
	DefaultMaxToolResultBytes = 262144
	// DefaultCompactReserveTokens is the token budget reserved for new
	// messages after compaction.
	DefaultCompactReserveTokens = 4000
	// DefaultCompactLastResort is the default compact_last_resort mode:
	// "condense" runs the last-resort condensation (Phase 0e) on a message
	// that cannot fit the context window; "error" returns a diagnostic
	// instead.
	DefaultCompactLastResort = "condense"
	// DefaultCommandTimeoutSecs is the maximum duration for execute_command.
	DefaultCommandTimeoutSecs = 120
	// DefaultSessionMaxCount is the maximum saved sessions per working dir.
	DefaultSessionMaxCount = 50
	// DefaultSessionMaxAgeDays is the retention window for saved sessions.
	DefaultSessionMaxAgeDays = 30
	// DefaultContextLimit is the fallback context window size (tokens) when
	// it cannot be resolved from the provider.
	DefaultContextLimit = 128000
	// DefaultWebMaxActiveSessions is the default cap on concurrently active
	// web sessions.
	DefaultWebMaxActiveSessions = 8
	// DefaultSubagentMaxDepth is the default maximum subagent nesting depth
	// (main agent = depth 0). 1 means subagents cannot spawn subagents.
	DefaultSubagentMaxDepth = 1
	// DefaultSubagentMaxConcurrent is the default cap on concurrently
	// running subagents per parent session (web host). Spawning beyond it
	// is refused; values <= 0 fall back to this default.
	DefaultSubagentMaxConcurrent = 4
)

// Effective returns v when v > 0, else def. It is the single "0 = unset, use
// the default" resolution rule for integer config values, so the policy is
// documented here once instead of drifting across call sites.
//
// NOTE on explicit zeros: this rule applies only where 0 is not a meaningful
// setting (session retention counts, ...). Some settings deliberately treat 0
// as meaningful and deliberately do NOT go through Effective:
//   - session retention (session.StoreOptions.MaxAgeDays): a NEGATIVE value
//     disables age-based pruning ("keep sessions forever"); 0 still means
//     the default via Effective;
//   - contextmgr.Settings: compact_threshold 0 disables auto-compaction,
//     max_tool_result_bytes 0 removes the truncation cap, compact_reserve_tokens
//     0 reserves nothing — only negative values fall back to defaults;
//   - projectfile.mergeIntOpt distinguishes an explicit file 0 from an absent
//     key via a non-nil pointer.
func Effective(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// MCPServerConfig describes one MCP stdio server entry. Tags cover both the
// JSON env-var form (GOGEN_MCP_SERVERS) and the YAML project-file form.
type MCPServerConfig struct {
	Name    string            `json:"name" yaml:"name"`
	Command string            `json:"command" yaml:"command"`
	Args    []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// OpenAIProviderConfig describes one registered OpenAI-compatible API
// endpoint: a base URL, an optional API key, and an optional default model.
// The legacy OpenAIKey/OpenAIModel/OpenAIURL fields form the implicit
// "default" profile; entries in Config.OpenAIProviders are additional
// registered providers whose models are aggregated into the web model
// picker and routed per model. Tags cover both the JSON env-var form
// (GOGEN_OPENAI_PROVIDERS) and the YAML project-file form.
type OpenAIProviderConfig struct {
	Name    string `json:"name" yaml:"name"`
	BaseURL string `json:"baseUrl" yaml:"base_url"`
	APIKey  string `json:"apiKey,omitempty" yaml:"api_key,omitempty"`
	Model   string `json:"model,omitempty" yaml:"model,omitempty"`
}

type Config struct {
	OpenAIKey   string
	OpenAIModel string
	OpenAIURL   string
	WorkingDir  string

	ContextLimit              int
	CompactThreshold          float64
	CompactKeepRecentMessages int
	MaxToolResultBytes        int
	CompactReserveTokens      int
	// CompactLastResort controls the last-resort condensation (Phase 0e)
	// for a message that cannot fit the context window even after all
	// compaction: "condense" (default) condenses the message in place via
	// the summarizer (the original is archived to the session's archive
	// sidecar and the condensation is announced in-band); "error" returns
	// a clear diagnostic instead of condensing.
	CompactLastResort string

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
	// OpenAIProviders lists additional registered OpenAI-compatible API
	// providers (see OpenAIProviderConfig). Empty means only the legacy
	// OpenAIKey/OpenAIModel/OpenAIURL default profile exists.
	OpenAIProviders []OpenAIProviderConfig

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
	// WebMaxActiveSessions caps concurrently active web sessions (registry
	// eviction bound; 0 = DefaultWebMaxActiveSessions).
	WebMaxActiveSessions int
	// WebApprovalHoldSecs is how long a pending delete approval survives the
	// last attached client detaching before it is auto-denied (0 = deny
	// immediately on detach, the default).
	WebApprovalHoldSecs int

	SessionMaxCount   int // max saved sessions per working dir (0 = default 50)
	SessionMaxAgeDays int // delete sessions older than N days (0 = default 30, negative = keep forever)

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

	// Board enables the project-wide kanban board feature ("on"/"off";
	// default off). When disabled the board tool is not registered and the
	// web board tab is hidden.
	Board string
	// Subagent enables the subagent tool that spawns nested sessions
	// ("on"/"off"; default off). When disabled the tool is not registered.
	Subagent string
	// SubagentMaxDepth is the maximum subagent nesting depth (main agent =
	// depth 0). The default 1 means subagents cannot spawn subagents; a
	// higher value re-enables nesting up to the configured depth. Values
	// <= 0 fall back to the default.
	SubagentMaxDepth int
	// SubagentMaxConcurrent is the maximum number of subagents that may run
	// concurrently for one parent session (web host; the TUI runs
	// foreground-only, so it is never bounded by this). The default 4
	// limits parallel fan-out; spawning beyond it is refused with an error
	// the model can act on (interrupt_agent / wait). Values <= 0 fall back
	// to the default.
	SubagentMaxConcurrent int
	// SubagentModel is the default model for spawned subagents. Empty
	// (the default) means subagents inherit the parent session's model;
	// an explicit tool-call model argument always wins over this value.
	SubagentModel string
	// SubagentThinkingLevel is the reasoning-effort level for spawned
	// subagents. Empty (the default) means subagents inherit the parent
	// session's live thinking level; "off" never sends reasoning_effort;
	// any other value is a literal reasoning_effort value, sent only when
	// the subagent's final model accepts it (omitted at spawn time
	// otherwise — the tool's model argument can override the configured
	// model, so validity is resolved against the child's model, never at
	// save time).
	SubagentThinkingLevel string

	// BoardStartPrompt is the template for prompts given to agents started
	// from a board ticket ("" = the built-in default). Placeholders:
	// {id} {title} {description} {priority} {context}.
	BoardStartPrompt string
	// SystemPrompt is a custom system prompt template ("" = the built-in
	// default). The {working_dir} placeholder is substituted with the
	// working directory; the project profile, project rules, and plan-mode
	// suffixes always append after it.
	SystemPrompt string
	// SubagentPrompt is the template wrapping subagent jobs ("" = the
	// built-in default). The {job} placeholder is substituted with the
	// tool call's job text.
	SubagentPrompt string
	// AgentInstructions enables loading AGENTS.md / CLAUDE.md workspace
	// instruction files ("on"/"off"; default off). When disabled the files
	// are never read; when enabled they are appended BELOW the project
	// guidelines (.gogen/gogen.md stays authoritative) with byte caps.
	AgentInstructions string
	// Skills enables the skill tool ("on"/"off"; default off). When
	// disabled the tool is not registered. Config-only in v1 (env/file —
	// no web settings toggle).
	Skills string
	// JobNotices enables background-job completion notices ("on"/"off";
	// default off): when a background shell job finishes naturally, a
	// summary is injected into the session as a user message and a turn
	// runs on it. Config-only in v1 (env/file — no web settings toggle).
	JobNotices string
}

// Defaults returns built-in default configuration values.
func Defaults() Config {
	return Config{
		OpenAIKey:                 "",
		OpenAIModel:               "",
		OpenAIURL:                 "",
		WorkingDir:                ".",
		ContextLimit:              0,
		CompactThreshold:          DefaultCompactThreshold,
		CompactKeepRecentMessages: DefaultCompactKeepRecentMessages,
		MaxToolResultBytes:        DefaultMaxToolResultBytes,
		CompactReserveTokens:      DefaultCompactReserveTokens,
		CompactLastResort:         DefaultCompactLastResort,
		CommandSafetyMode:         "blocklist",
		CommandAllowlist:          "",
		DeleteApproval:            "required",
		TreeSitter:                "on",
		TreeSitterLangs:           "",
		CLIVerbose:                false,
		DebugLog:                  "",
		DebugSession:              "",
		MCP:                       "off",
		DebugCompareMessages:      false,
		WebBind:                   "127.0.0.1:8081",
		WebAllowedOrigins:         "",
		WebTLSCertFile:            "",
		WebTLSKeyFile:             "",
		SessionMaxCount:           DefaultSessionMaxCount,
		SessionMaxAgeDays:         DefaultSessionMaxAgeDays,
		WebMaxActiveSessions:      DefaultWebMaxActiveSessions,
		WebApprovalHoldSecs:       0,
		WebFetch:                  "on",
		WebSearch:                 "on",
		WebSearchBackend:          "",
		WebSearchAPIKey:           "",
		WebAllowedDomains:         "",
		WebFetchMode:              "https",
		CommandSandbox:            "off",
		CommandTimeoutSecs:        DefaultCommandTimeoutSecs,
		PreserveReasoning:         "auto",
		Board:                     "off",
		Subagent:                  "off",
		SubagentMaxDepth:          DefaultSubagentMaxDepth,
		SubagentMaxConcurrent:     DefaultSubagentMaxConcurrent,
		AgentInstructions:         "off",
		Skills:                    "off",
		JobNotices:                "off",
	}
}

// ApprovalHold returns the configured approval-hold duration. Zero means
// "deny pending approvals immediately when the last client detaches".
func (c *Config) ApprovalHold() time.Duration {
	if c == nil || c.WebApprovalHoldSecs <= 0 {
		return 0
	}
	return time.Duration(c.WebApprovalHoldSecs) * time.Second
}

// configOn reports whether a config field is explicitly enabled.
func configOn(v string) bool {
	return onoff.Enabled(v)
}

// configOff reports whether a config field is explicitly disabled
// (set to "off", "0", "false", or "no"). An empty string is not "off" —
// absence of a value does not imply explicit disablement.
func configOff(v string) bool {
	on, ok := onoff.Parse(v)
	return ok && !on
}

// MCPEnabled reports whether MCP integration is active.
// Opt-in: servers in project config are not started unless mcp is explicitly enabled.
func (c *Config) MCPEnabled() bool {
	return c != nil && configOn(c.MCP)
}

// BoardEnabled reports whether the project kanban board feature is active.
// Opt-in: the board tool is not registered and the web board tab is hidden
// unless board is explicitly enabled.
func (c *Config) BoardEnabled() bool {
	return c != nil && configOn(c.Board)
}

// SubagentEnabled reports whether the subagent tool is active.
// Opt-in: the tool is not registered unless subagent is explicitly enabled.
func (c *Config) SubagentEnabled() bool {
	return c != nil && configOn(c.Subagent)
}

// AgentInstructionsEnabled reports whether AGENTS.md/CLAUDE.md workspace
// instruction files are loaded. Opt-in: the files are never read unless
// agent_instructions is explicitly enabled.
func (c *Config) AgentInstructionsEnabled() bool {
	return c != nil && configOn(c.AgentInstructions)
}

// SkillsEnabled reports whether the skill tool is active. Opt-in: the tool
// is not registered unless skills is explicitly enabled.
func (c *Config) SkillsEnabled() bool {
	return c != nil && configOn(c.Skills)
}

// JobNoticesEnabled reports whether background-job completion notices are
// active. Opt-in: no notice hook is installed unless job_notices is
// explicitly enabled.
func (c *Config) JobNoticesEnabled() bool {
	return c != nil && configOn(c.JobNotices)
}

// SubagentDepth returns the effective maximum subagent nesting depth
// (main agent = depth 0). Values <= 0 fall back to DefaultSubagentMaxDepth.
func (c *Config) SubagentDepth() int {
	if c == nil || c.SubagentMaxDepth <= 0 {
		return DefaultSubagentMaxDepth
	}
	return c.SubagentMaxDepth
}

// SubagentLimit returns the effective maximum number of concurrently
// running subagents per parent session. Values <= 0 fall back to
// DefaultSubagentMaxConcurrent.
func (c *Config) SubagentLimit() int {
	if c == nil || c.SubagentMaxConcurrent <= 0 {
		return DefaultSubagentMaxConcurrent
	}
	return c.SubagentMaxConcurrent
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
